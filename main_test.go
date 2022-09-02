package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	coreType "k8s.io/client-go/kubernetes/typed/core/v1"
	"log"
	"os"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/doddle/registry-creds/k8sutil"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	// v1 "k8s.io/client-go/pkg/api/v1"
	v1 "k8s.io/api/core/v1"
	//"k8s.io/client-go/pkg/watch"
)

func init() {
	log.SetOutput(io.Discard)
	logrus.SetOutput(io.Discard)
}

func enableShortRetries() {
	RetryCfg = RetryConfig{
		Type:                "simple",
		NumberOfRetries:     2,
		RetryDelayInSeconds: 1,
	}
	SetupRetryTimer()
}

type fakeKubeClient struct {
	secrets         map[string]*fakeSecrets
	namespaces      *fakeNamespaces
	serviceaccounts map[string]*fakeServiceAccounts
}

func (f *fakeKubeClient) Secrets(namespace string) coreType.SecretInterface {
	return f.secrets[namespace]
}

func (f *fakeKubeClient) Core() coreType.CoreV1Interface {
	return f.Core()
}

type fakeSecrets struct {
	coreType.SecretInterface
	store map[string]*v1.Secret
}

type fakeServiceAccounts struct {
	coreType.ServiceAccountInterface
	store map[string]*v1.ServiceAccount
}

func (f *fakeServiceAccounts) Update(ctx context.Context, serviceAccount *v1.ServiceAccount, opts metav1.UpdateOptions) (*v1.ServiceAccount, error) {
	serviceAccount, ok := f.store[serviceAccount.Name]

	if !ok {
		return nil, fmt.Errorf("service account '%v' not found", serviceAccount.Name)
	}

	f.store[serviceAccount.Name] = serviceAccount
	return serviceAccount, nil
}

func (f *fakeServiceAccounts) Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error {
	_, ok := f.store[name]

	if !ok {
		return fmt.Errorf("service account '%v' not found", name)
	}

	delete(f.store, name)
	return nil
}

func (f *fakeServiceAccounts) Get(ctx context.Context, name string, opts metav1.GetOptions) (*v1.ServiceAccount, error) {
	serviceAccount, ok := f.store[name]

	if !ok {
		return nil, fmt.Errorf("failed to find service account '%v'", name)
	}

	return serviceAccount, nil
}

type fakeNamespaces struct {
	coreType.NamespaceInterface
	store map[string]v1.Namespace
}

func (f *fakeNamespaces) List(ctx context.Context, opts metav1.ListOptions) (*v1.NamespaceList, error) {
	namespaces := make([]v1.Namespace, 0)

	for _, v := range f.store {
		namespaces = append(namespaces, v)
	}

	return &v1.NamespaceList{Items: namespaces}, nil
}

func (f *fakeKubeClient) Namespaces() coreType.NamespaceInterface {
	return f.namespaces
}

func (f *fakeKubeClient) ServiceAccounts(namespace string) coreType.ServiceAccountInterface {
	return f.serviceaccounts[namespace]
}

func (f *fakeSecrets) Create(ctx context.Context, secret *v1.Secret, opts metav1.CreateOptions) (*v1.Secret, error) {
	_, ok := f.store[secret.Name]

	if ok {
		return nil, fmt.Errorf("secret %v already exists", secret.Name)
	}

	f.store[secret.Name] = secret
	return secret, nil
}

func (f *fakeSecrets) Update(ctx context.Context, secret *v1.Secret, opts metav1.UpdateOptions) (*v1.Secret, error) {
	_, ok := f.store[secret.Name]

	if !ok {
		return nil, fmt.Errorf("secret %v not found", secret.Name)
	}

	f.store[secret.Name] = secret
	return secret, nil
}

func (f *fakeSecrets) Get(ctx context.Context, name string, opts metav1.GetOptions) (*v1.Secret, error) {
	secret, ok := f.store[name]

	if !ok {
		return nil, fmt.Errorf("secret with name '%v' not found", name)
	}

	return secret, nil
}

type fakeEcrClient struct{}

func (f *fakeEcrClient) GetAuthorizationToken(input *ecr.GetAuthorizationTokenInput) (*ecr.GetAuthorizationTokenOutput, error) {
	if len(input.RegistryIds) == 2 {
		return &ecr.GetAuthorizationTokenOutput{
			AuthorizationData: []*ecr.AuthorizationData{
				{
					AuthorizationToken: aws.String("fakeToken1"),
					ProxyEndpoint:      aws.String("fakeEndpoint1"),
				},
				{
					AuthorizationToken: aws.String("fakeToken2"),
					ProxyEndpoint:      aws.String("fakeEndpoint2"),
				},
			},
		}, nil
	}
	return &ecr.GetAuthorizationTokenOutput{
		AuthorizationData: []*ecr.AuthorizationData{
			{
				AuthorizationToken: aws.String("fakeToken"),
				ProxyEndpoint:      aws.String("fakeEndpoint"),
			},
		},
	}, nil
}

type fakeFailingEcrClient struct{}

func (f *fakeFailingEcrClient) GetAuthorizationToken(input *ecr.GetAuthorizationTokenInput) (*ecr.GetAuthorizationTokenOutput, error) {
	return nil, errors.New("fake error")
}

func newKubeUtil() *k8sutil.KubeUtilInterface {
	return &k8sutil.KubeUtilInterface{
		Kclient: newFakeKubeClient(),
	}
}

func newFakeKubeClient() k8sutil.KubeInterface {
	return &fakeKubeClient{
		secrets: map[string]*fakeSecrets{
			"namespace1": {
				store: map[string]*v1.Secret{},
			},
			"namespace2": {
				store: map[string]*v1.Secret{},
			},
			"kube-system": {
				store: map[string]*v1.Secret{},
			},
		},
		namespaces: &fakeNamespaces{store: map[string]v1.Namespace{
			"namespace1": {
				ObjectMeta: metav1.ObjectMeta{
					Name: "namespace1",
				},
			},
			"namespace2": {
				ObjectMeta: metav1.ObjectMeta{
					Name: "namespace2",
				},
			},
			"kube-system": {
				ObjectMeta: metav1.ObjectMeta{
					Name: "kube-system",
				},
			},
		}},
		serviceaccounts: map[string]*fakeServiceAccounts{
			"namespace1": {
				store: map[string]*v1.ServiceAccount{
					"default": {
						ObjectMeta: metav1.ObjectMeta{
							Name: "default",
						},
					},
				},
			},
			"namespace2": {
				store: map[string]*v1.ServiceAccount{
					"default": {
						ObjectMeta: metav1.ObjectMeta{
							Name: "default",
						},
					},
				},
			},
			"kube-system": {
				store: map[string]*v1.ServiceAccount{
					"default": {
						ObjectMeta: metav1.ObjectMeta{
							Name: "default",
						},
					},
				},
			},
		},
	}
}

func newFakeEcrClient() *fakeEcrClient {
	return &fakeEcrClient{}
}

func newFakeFailingEcrClient() *fakeFailingEcrClient {
	return &fakeFailingEcrClient{}
}

func process(t *testing.T, c *controller) {
	namespaces, _ := c.k8sutil.Kclient.Namespaces().List(context.TODO(), metav1.ListOptions{})
	for _, ns := range namespaces.Items {
		err := handler(c, &ns)
		assert.Nil(t, err)
	}
}

func newFakeController() *controller {
	util := newKubeUtil()
	ecrClient := newFakeEcrClient()
	c := controller{util, ecrClient}
	return &c
}

func newFakeFailingController() *controller {
	util := newKubeUtil()
	ecrClient := newFakeFailingEcrClient()
	c := controller{util, ecrClient}
	return &c
}

func TestGetECRAuthorizationKey(t *testing.T) {
	awsAccountIDs = []string{"12345678", "999999"}
	c := newFakeController()

	tokens, err := c.getECRAuthorizationKey()

	assert.Nil(t, err)
	assert.Equal(t, 2, len(tokens))
	assert.Equal(t, "fakeToken1", tokens[0].AccessToken)
	assert.Equal(t, "fakeEndpoint1", tokens[0].Endpoint)
	assert.Equal(t, "fakeToken2", tokens[1].AccessToken)
	assert.Equal(t, "fakeEndpoint2", tokens[1].Endpoint)
}

func assertDockerJSONContains(t *testing.T, endpoint, token string, secret *v1.Secret) {
	d := dockerJSON{}
	assert.Nil(t, json.Unmarshal(secret.Data[".dockerconfigjson"], &d))
	assert.Contains(t, d.Auths, endpoint)
	assert.Equal(t, d.Auths[endpoint].Auth, token)
	assert.Equal(t, d.Auths[endpoint].Email, "none")
}

func assertSecretPresent(t *testing.T, secrets []v1.LocalObjectReference, name string) {
	for _, s := range secrets {
		if s.Name == name {
			return
		}
	}
	assert.Failf(t, "ImagePullSecrets validation failed", "Expected secret %v not present", name)
}

func assertAllSecretsPresent(t *testing.T, secrets []v1.LocalObjectReference) {
	assertSecretPresent(t, secrets, *argAWSSecretName)
}

func assertAllExpectedSecrets(t *testing.T, c *controller) {
	// Test AWS
	for _, ns := range []string{"namespace1", "namespace2"} {
		secret, err := c.k8sutil.GetSecret(ns, *argAWSSecretName)
		assert.Nil(t, err)
		assert.Equal(t, *argAWSSecretName, secret.Name)
		assertDockerJSONContains(t, "fakeEndpoint", "fakeToken", secret)
		assert.Equal(t, v1.SecretType("kubernetes.io/dockerconfigjson"), secret.Type)
	}

	_, err := c.k8sutil.GetSecret("kube-system", *argAWSSecretName)
	assert.NotNil(t, err)
}

func assertExpectedSecretNumber(t *testing.T, c *controller, n int) {
	for _, ns := range []string{"namespace1", "namespace2"} {
		serviceAccount, err := c.k8sutil.GetServiceAccount(ns, "default")
		assert.Nil(t, err)
		assert.Exactly(t, n, len(serviceAccount.ImagePullSecrets))
	}
}

func TestProcessOnce(t *testing.T) {
	awsAccountIDs = []string{""}
	c := newFakeController()

	process(t, c)

	assertAllExpectedSecrets(t, c)
}

func TestProcessTwice(t *testing.T) {
	c := newFakeController()

	process(t, c)
	// test processing twice for idempotency
	process(t, c)

	assertAllExpectedSecrets(t, c)

	// Verify that secrets have not been created twice
	assertExpectedSecretNumber(t, c, 1)
}

func TestProcessWithExistingSecrets(t *testing.T) {
	c := newFakeController()

	secretAWS := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: *argAWSSecretName,
		},
		Data: map[string][]byte{
			".dockerconfigjson": []byte("some other config"),
		},
		Type: "some other type",
	}

	for _, ns := range []string{"namespace1", "namespace2"} {
		for _, secret := range []*v1.Secret{
			secretAWS,
		} {
			err := c.k8sutil.CreateSecret(ns, secret)
			assert.Nil(t, err)
		}
	}

	process(t, c)

	assertAllExpectedSecrets(t, c)
	assertExpectedSecretNumber(t, c, 1)
}

func TestProcessWithExistingImagePullSecrets(t *testing.T) {
	c := newFakeController()

	for _, ns := range []string{"namespace1", "namespace2"} {
		serviceAccount, err := c.k8sutil.GetServiceAccount(ns, "default")
		assert.Nil(t, err)
		serviceAccount.ImagePullSecrets = append(serviceAccount.ImagePullSecrets, v1.LocalObjectReference{Name: "someOtherSecret"})
		_ = c.k8sutil.UpdateServiceAccount(ns, serviceAccount)
	}

	process(t, c)

	for _, ns := range []string{"namespace1", "namespace2"} {
		serviceAccount, err := c.k8sutil.GetServiceAccount(ns, "default")
		assert.Nil(t, err)
		assertAllSecretsPresent(t, serviceAccount.ImagePullSecrets)
		assertSecretPresent(t, serviceAccount.ImagePullSecrets, "someOtherSecret")
	}
}

func TestDefaultAwsRegionFromArgs(t *testing.T) {
	assert.Equal(t, "us-east-1", *argAWSRegion)
}

func TestAwsRegionFromEnv(t *testing.T) {
	expectedRegion := "us-steve-1"

	_ = os.Setenv("awsaccount", "12345678")
	_ = os.Setenv("awsregion", expectedRegion)
	validateParams()

	assert.Equal(t, expectedRegion, *argAWSRegion)
}

func TestFailingGcrPassingEcrStillSucceeds(t *testing.T) {
	enableShortRetries()

	awsAccountIDs = []string{""}
	c := newFakeFailingController()
	c.ecrClient = newFakeEcrClient()

	process(t, c)
}

func TestControllerGenerateSecretsSimpleRetryOnError(t *testing.T) {
	// enable log output for this test
	log.SetOutput(os.Stdout)
	logrus.SetOutput(os.Stdout)
	// disable log output when the test has completed
	defer func() {
		log.SetOutput(io.Discard)
		logrus.SetOutput(io.Discard)
	}()
	enableShortRetries()

	awsAccountIDs = []string{""}
	c := newFakeFailingController()

	process(t, c)
}

func TestControllerGenerateSecretsExponentialRetryOnError(t *testing.T) {
	// enable log output for this test
	log.SetOutput(os.Stdout)
	logrus.SetOutput(os.Stdout)
	// disable log output when the test has completed
	defer func() {
		log.SetOutput(io.Discard)
		logrus.SetOutput(io.Discard)
	}()
	RetryCfg = RetryConfig{
		Type:                "exponential",
		NumberOfRetries:     3,
		RetryDelayInSeconds: 1,
	}
	SetupRetryTimer()
	awsAccountIDs = []string{""}
	c := newFakeFailingController()

	process(t, c)
}
