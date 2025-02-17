/*
Copyright (c) 2017, UPMC Enterprises
All rights reserved.
Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are met:
    * Redistributions of source code must retain the above copyright
      notice, this list of conditions and the following disclaimer.
    * Redistributions in binary form must reproduce the above copyright
      notice, this list of conditions and the following disclaimer in the
      documentation and/or other materials provided with the distribution.
    * Neither the name UPMC Enterprises nor the
      names of its contributors may be used to endorse or promote products
      derived from this software without specific prior written permission.
THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS" AND
ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE IMPLIED
WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
DISCLAIMED. IN NO EVENT SHALL UPMC ENTERPRISES BE LIABLE FOR ANY
DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES
(INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES;
LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND
ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
(INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE OF THIS
*/

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials/stscreds"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/cenkalti/backoff"
	"github.com/doddle/registry-creds/k8sutil"
	log "github.com/sirupsen/logrus"
	flag "github.com/spf13/pflag"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func init() {
	// TODO: this is so bad as a global :(
	// log.SetFormatter(&log.JSONFormatter{})
	log.SetOutput(os.Stdout)
	log.SetReportCaller(true)

	log.SetFormatter(&log.TextFormatter{
		CallerPrettyfier: func(f *runtime.Frame) (string, string) {
			filename := path.Base(f.File)
			return fmt.Sprintf("%s()", f.Function), fmt.Sprintf("%s:%d", filename, f.Line)
		},
	})
}

const (
	// Retry Types
	retryTypeSimple      = "simple"
	retryTypeExponential = "exponential"

	dockerCfgTemplate         = `{"%s":{"username":"oauth2accesstoken","password":"%s","email":"none"}}`
	tokenGenRetryTypeKey      = "TOKEN_RETRY_TYPE"
	tokenGenRetriesKey        = "TOKEN_RETRIES"
	tokenGenRetryDelayKey     = "TOKEN_RETRY_DELAY"
	defaultTokenGenRetries    = 3
	defaultTokenGenRetryDelay = 5 // in seconds
	defaultTokenGenRetryType  = retryTypeSimple
)

var (
	flags                    = flag.NewFlagSet("", flag.ContinueOnError)
	argExcludedNamespaces    = flags.String("excluded-namespaces", "", `Comma seperated list of namespaces that do NOT need updated secrets`)
	argAWSSecretName         = flags.String("aws-secret-name", "awsecr-cred", `Default AWS secret name`)
	argAWSRegion             = flags.String("aws-region", "us-east-1", `Default AWS region`)
	argRefreshMinutes        = flags.Int("refresh-mins", 60, `Default time to wait before refreshing (60 minutes)`)
	argSkipKubeSystem        = flags.Bool("skip-kube-system", true, `If true, will not attempt to set ImagePullSecrets on the kube-system namespace`)
	argAWSAssumeRole         = flags.String("aws_assume_role", "", `If specified AWS will assume this role and use it to retrieve tokens`)
	argTokenGenFxnRetryType  = flags.String("token-retry-type", defaultTokenGenRetryType, `The type of retry timer to use when generating a secret token; either simple or exponential (simple)`)
	argTokenGenFxnRetries    = flags.Int("token-retries", defaultTokenGenRetries, `Default number of times to retry generating a secret token (3)`)
	argTokenGenFxnRetryDelay = flags.Int("token-retry-delay", defaultTokenGenRetryDelay, `Default number of seconds to wait before retrying secret token generation (5 seconds)`)
)

var (
	awsAccountIDs []string

	// RetryCfg represents the currently-configured number of retries + retry delay
	RetryCfg RetryConfig

	// The retry backoff timers
	simpleBackoff      *backoff.ConstantBackOff
	exponentialBackoff *backoff.ExponentialBackOff
)

type dockerJSON struct {
	Auths map[string]registryAuth `json:"auths,omitempty"`
}

type registryAuth struct {
	Auth  string `json:"auth"`
	Email string `json:"email"`
}

type controller struct {
	k8sutil   *k8sutil.KubeUtilInterface
	ecrClient ecrInterface
}

// RetryConfig represents the number of retries + the retry delay for retrying an operation if it should fail
type RetryConfig struct {
	Type                string
	NumberOfRetries     int
	RetryDelayInSeconds int
}

type ecrInterface interface {
	GetAuthorizationToken(input *ecr.GetAuthorizationTokenInput) (*ecr.GetAuthorizationTokenOutput, error)
}

func newEcrClient() ecrInterface {
	sess := session.Must(session.NewSession())
	awsConfig := aws.NewConfig().WithRegion(*argAWSRegion)

	if *argAWSAssumeRole != "" {
		creds := stscreds.NewCredentials(sess, *argAWSAssumeRole)
		awsConfig.Credentials = creds
	}

	return ecr.New(sess, awsConfig)
}

func (c *controller) getECRAuthorizationKey() ([]AuthToken, error) {
	var tokens []AuthToken

	regIds := make([]*string, len(awsAccountIDs))
	for i, awsAccountID := range awsAccountIDs {
		regIds[i] = aws.String(awsAccountID)
	}

	params := &ecr.GetAuthorizationTokenInput{
		RegistryIds: regIds,
	}

	resp, err := c.ecrClient.GetAuthorizationToken(params)

	if err != nil {
		// Print the error, cast err to awserr.Error to get the Code and
		// Message from an error.
		log.Println(err.Error())
		return []AuthToken{}, err
	}

	for _, auth := range resp.AuthorizationData {
		tokens = append(tokens, AuthToken{
			AccessToken: *auth.AuthorizationToken,
			Endpoint:    *auth.ProxyEndpoint,
		})
	}

	return tokens, nil
}

func generateSecretObj(tokens []AuthToken, isJSONCfg bool, secretName string) (*v1.Secret, error) {
	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: secretName,
		},
	}
	if isJSONCfg {
		auths := map[string]registryAuth{}
		for _, token := range tokens {
			auths[token.Endpoint] = registryAuth{
				Auth:  token.AccessToken,
				Email: "none",
			}
		}
		configJSON, err := json.Marshal(dockerJSON{Auths: auths})
		if err != nil {
			return secret, err
		}
		secret.Data = map[string][]byte{".dockerconfigjson": configJSON}
		secret.Type = "kubernetes.io/dockerconfigjson"
	} else if len(tokens) == 1 {
		secret.Data = map[string][]byte{
			".dockercfg": []byte(fmt.Sprintf(dockerCfgTemplate, tokens[0].Endpoint, tokens[0].AccessToken))}
		secret.Type = "kubernetes.io/dockercfg"
	}
	return secret, nil
}

// AuthToken represents an Access Token and an Endpoint for a registry service
type AuthToken struct {
	AccessToken string
	Endpoint    string
}

// SecretGenerator represents a token generation function for a registry service
type SecretGenerator struct {
	TokenGenFxn func() ([]AuthToken, error)
	IsJSONCfg   bool
	SecretName  string
}

func getSecretGenerators(c *controller) []SecretGenerator {
	secretGenerators := make([]SecretGenerator, 0)

	secretGenerators = append(secretGenerators, SecretGenerator{
		TokenGenFxn: c.getECRAuthorizationKey,
		IsJSONCfg:   true,
		SecretName:  *argAWSSecretName,
	})

	return secretGenerators
}

func (c *controller) processNamespace(namespace *v1.Namespace, secret *v1.Secret) error {
	logw := log.WithField("function", "processNamespace")
	// Check if the secret exists for the namespace
	logw.Debugf("checking for secret %s in namespace %s", secret.Name, namespace.GetName())
	_, err := c.k8sutil.GetSecret(namespace.GetName(), secret.Name)

	if err != nil {
		logw.Debugf("Could not find secret %s in namespace %s; will try to create it", secret.Name, namespace.GetName())
		// Secret not found, create
		err := c.k8sutil.CreateSecret(namespace.GetName(), secret)
		if err != nil {
			return fmt.Errorf("could not create Secret: %v", err)
		}
		logw.Infof("Created new secret %s in namespace %s", secret.Name, namespace.GetName())
	} else {
		// Existing secret needs updated
		logw.Debugf("Found secret %s in namespace %s; will try to update it", secret.Name, namespace.GetName())
		err := c.k8sutil.UpdateSecret(namespace.GetName(), secret)
		if err != nil {
			return fmt.Errorf("could not update Secret: %v", err)
		}
		logw.Infof("Updated secret %s in namespace %s", secret.Name, namespace.GetName())
	}

	// Check if ServiceAccount exists
	serviceAccount, err := c.k8sutil.GetServiceAccount(namespace.GetName(), "default")
	if err != nil {
		logw.Errorf("error getting service account default in namespace %s: %s", namespace.GetName(), err)
		return fmt.Errorf("could not get ServiceAccounts: %v", err)
	}

	// Update existing one if image pull secrets already exists for aws ecr token
	imagePullSecretFound := false
	for i, imagePullSecret := range serviceAccount.ImagePullSecrets {
		if imagePullSecret.Name == secret.Name {
			serviceAccount.ImagePullSecrets[i] = v1.LocalObjectReference{Name: secret.Name}
			imagePullSecretFound = true
			break
		}
	}

	// Append to list of existing service accounts if there isn't one already
	if !imagePullSecretFound {
		serviceAccount.ImagePullSecrets = append(serviceAccount.ImagePullSecrets, v1.LocalObjectReference{Name: secret.Name})
	}

	logw.Infof("Updating ServiceAccount %s in namespace %s", serviceAccount.Name, namespace.GetName())
	err = c.k8sutil.UpdateServiceAccount(namespace.GetName(), serviceAccount)
	if err != nil {
		logw.Errorf("error updating ServiceAccount %s in namespace %s: %s", serviceAccount.Name, namespace.GetName(), err)
		return fmt.Errorf("could not update ServiceAccount: %v", err)
	}

	return nil
}

func (c *controller) generateSecrets() []*v1.Secret {
	var secrets []*v1.Secret
	secretGenerators := getSecretGenerators(c)

	maxTries := RetryCfg.NumberOfRetries + 1
	for _, secretGenerator := range secretGenerators {
		resetRetryTimer()

		var newTokens []AuthToken
		tries := 0
		for {
			tries++
			log.Infof("Getting secret; try #%d of %d", tries, maxTries)
			tokens, err := secretGenerator.TokenGenFxn()
			if err != nil {
				if tries < maxTries {
					delayDuration := nextRetryDuration()
					if delayDuration == backoff.Stop {
						log.Errorf("Error getting secret for provider %s. Retry timer exceeded max tries/duration; will not try again until the next refresh cycle. [Err: %s]", secretGenerator.SecretName, err)
						break
					}
					log.Errorf("Error getting secret for provider %s. Will try again after %f seconds. [Err: %s]", secretGenerator.SecretName, delayDuration.Seconds(), err)
					<-time.After(delayDuration)
					continue
				}
				log.Errorf("Error getting secret for provider %s. Tried %d time(s); will not try again until the next refresh cycle. [Err: %s]", secretGenerator.SecretName, tries, err)
				// os.Exit(1)
				break
			} else {
				log.Infof("Successfully got secret for provider %s after trying %d time(s)", secretGenerator.SecretName, tries)
				newTokens = tokens
				break
			}
		}

		newSecret, err := generateSecretObj(newTokens, secretGenerator.IsJSONCfg, secretGenerator.SecretName)
		if err != nil {
			log.Errorf("Error generating secret for provider %s. Skipping secret provider until the next refresh cycle! [Err: %s]", secretGenerator.SecretName, err)
		} else {
			secrets = append(secrets, newSecret)
		}
	}
	return secrets
}

// SetupRetryTimer initializes and configures the Retry Timer
func SetupRetryTimer() {
	delayDuration := time.Duration(RetryCfg.RetryDelayInSeconds) * time.Second
	switch RetryCfg.Type {
	case retryTypeSimple:
		simpleBackoff = backoff.NewConstantBackOff(delayDuration)
	case retryTypeExponential:
		exponentialBackoff = backoff.NewExponentialBackOff()
	}
}

func resetRetryTimer() {
	switch RetryCfg.Type {
	case retryTypeSimple:
		simpleBackoff.Reset()
	case retryTypeExponential:
		exponentialBackoff.Reset()
	}
}

func nextRetryDuration() time.Duration {
	switch RetryCfg.Type {
	case retryTypeSimple:
		return simpleBackoff.NextBackOff()
	case retryTypeExponential:
		return exponentialBackoff.NextBackOff()
	default:
		return time.Duration(defaultTokenGenRetryDelay) * time.Second
	}
}

func validateParams() {
	// Allow environment variables to overwrite args
	awsAccountIDEnv := os.Getenv("awsaccount")
	awsRegionEnv := os.Getenv("awsregion")
	argAWSAssumeRoleEnv := os.Getenv("aws_assume_role")

	// initialize the retry configuration using command line values
	RetryCfg = RetryConfig{
		Type:                *argTokenGenFxnRetryType,
		NumberOfRetries:     *argTokenGenFxnRetries,
		RetryDelayInSeconds: *argTokenGenFxnRetryDelay,
	}
	// ensure command line values are valid
	if RetryCfg.Type != retryTypeSimple && RetryCfg.Type != retryTypeExponential {
		log.Errorf("Unknown Retry Timer type '%s'! Defaulting to %s", RetryCfg.Type, defaultTokenGenRetryType)
		RetryCfg.Type = defaultTokenGenRetryType
	}
	if RetryCfg.NumberOfRetries < 0 {
		log.Errorf("Cannot use a negative value for the number of retries! Defaulting to %d", defaultTokenGenRetries)
		RetryCfg.NumberOfRetries = defaultTokenGenRetries
	}
	if RetryCfg.RetryDelayInSeconds < 0 {
		log.Errorf("Cannot use a negative value for the retry delay in seconds! Defaulting to %d", defaultTokenGenRetryDelay)
		RetryCfg.RetryDelayInSeconds = defaultTokenGenRetryDelay
	}
	// look for overrides in environment variables and use them if they exist and are valid
	tokenType, ok := os.LookupEnv(tokenGenRetryTypeKey)
	if ok && len(tokenType) > 0 {
		if tokenType != retryTypeSimple && tokenType != retryTypeExponential {
			log.Errorf("Unknown Retry Timer type '%s'! Defaulting to %s", tokenType, defaultTokenGenRetryType)
			RetryCfg.Type = defaultTokenGenRetryType
		} else {
			RetryCfg.Type = tokenType
		}
	}
	tokenRetries, ok := os.LookupEnv(tokenGenRetriesKey)
	if ok && len(tokenRetries) > 0 {
		tokenRetriesInt, err := strconv.Atoi(tokenRetries)
		if err != nil {
			log.Errorf("Unable to parse value of environment variable %s! [Err: %s]", tokenGenRetriesKey, err)
		} else {
			if tokenRetriesInt < 0 {
				log.Errorf("Cannot use a negative value for environment variable %s! Defaulting to %d", tokenGenRetriesKey, defaultTokenGenRetries)
				RetryCfg.NumberOfRetries = defaultTokenGenRetries
			} else {
				RetryCfg.NumberOfRetries = tokenRetriesInt
			}
		}
	}
	tokenRetryDelay, ok := os.LookupEnv(tokenGenRetryDelayKey)
	if ok && len(tokenRetryDelay) > 0 {
		tokenRetryDelayInt, err := strconv.Atoi(tokenRetryDelay)
		if err != nil {
			log.Errorf("Unable to parse value of environment variable %s! [Err: %s]", tokenGenRetryDelayKey, err)
		} else {
			if tokenRetryDelayInt < 0 {
				log.Errorf("Cannot use a negative value for environment variable %s! Defaulting to %d", tokenGenRetryDelayKey, defaultTokenGenRetryDelay)
				RetryCfg.RetryDelayInSeconds = defaultTokenGenRetryDelay
			} else {
				RetryCfg.RetryDelayInSeconds = tokenRetryDelayInt
			}
		}
	}
	// Set up the Retry Timer
	SetupRetryTimer()

	if len(awsRegionEnv) > 0 {
		argAWSRegion = &awsRegionEnv
	}

	if len(awsAccountIDEnv) > 0 {
		awsAccountIDs = strings.Split(awsAccountIDEnv, ",")
	} else {
		awsAccountIDs = []string{""}
	}

	if len(argAWSAssumeRoleEnv) > 0 {
		argAWSAssumeRole = &argAWSAssumeRoleEnv
	}
}

func stringSliceContains(stringSlice []string, searchString string) bool {
	// simple check to see if a slice of strings has one matching the input string
	for _, v := range stringSlice {
		if v == searchString {
			return true
		}
	}
	return false
}

func handler(c *controller, ns *v1.Namespace) error {
	namespace := ns.GetName()
	if stringSliceContains(c.k8sutil.ExcludedNamespaces, namespace) {
		log.Infof("---------- handler( namespace: %s excluded)", namespace)
		return nil
	}
	log.Infof("---------- handler( namespace: %s started)", namespace)
	log.Infof("generating credentials for namespace %s", namespace)
	secrets := c.generateSecrets()
	log.Infof("Got %d refreshed credentials for namespace %s", len(secrets), namespace)
	for _, secret := range secrets {
		if *argSkipKubeSystem && namespace == "kube-system" {
			continue
		}
		log.Infof("Processing secret for namespace %s, secret %s", ns.Name, secret.Name)

		if err := c.processNamespace(ns, secret); err != nil {
			log.Errorf("error processing secret for namespace %s, secret %s: %s", ns.Name, secret.Name, err)
			return err
		}

		log.Infof("Finished processing secret for namespace %s, secret %s", ns.Name, secret.Name)
	}
	log.Infof("Finished refreshing credentials for namespace %s", ns.GetName())
	return nil
}

func main() {
	log.Info("Starting up...")
	err := flags.Parse(os.Args)
	if err != nil {
		log.Fatalf("Could not parse command line arguments! [Err: %s]", err)
	}

	validateParams()

	log.Info("Using AWS Account: ", strings.Join(awsAccountIDs, ","))
	log.Info("Using AWS Region: ", *argAWSRegion)
	log.Info("Using AWS Assume Role: ", *argAWSAssumeRole)
	log.Info("Refresh Interval (minutes): ", *argRefreshMinutes)
	log.Infof("Retry Timer: %s", RetryCfg.Type)
	log.Info("Token Generation Retries: ", RetryCfg.NumberOfRetries)
	log.Info("Token Generation Retry Delay (seconds): ", RetryCfg.RetryDelayInSeconds)

	excludedNamespaces := strings.Split(*argExcludedNamespaces, ",")
	util, err := k8sutil.New(excludedNamespaces)
	if err != nil {
		log.Error("Could not create k8s client!!", err)
	}

	ecrClient := newEcrClient()
	c := &controller{util, ecrClient}

	util.WatchNamespaces(time.Duration(*argRefreshMinutes)*time.Minute, func(ns *v1.Namespace) error {
		return handler(c, ns)
	})
}
