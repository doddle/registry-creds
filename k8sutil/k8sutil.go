package k8sutil

import (
	"context"
	"k8s.io/apimachinery/pkg/fields"
	"log"
	"os"
	"time"

	"fmt"
	"github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes"
	coreType "k8s.io/client-go/kubernetes/typed/core/v1"
	// "k8s.io/client-go/pkg/api/v1"
	//"k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	// "k8s.io/client-go/pkg/fields"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"path/filepath"
)

// KubeInterface abstracts the k8s api
type KubeInterface interface {
	Secrets(namespace string) coreType.SecretInterface
	Namespaces() coreType.NamespaceInterface
	ServiceAccounts(namespace string) coreType.ServiceAccountInterface
	Core() coreType.CoreV1Interface
}

type KubeUtilInterface struct {
	Kclient            KubeInterface
	ExcludedNamespaces []string
}

// New creates a new instance of k8sutil
func New(excludedNamespaces []string) (*KubeUtilInterface, error) {
	client, err := newKubeClient()

	if err != nil {
		logrus.Fatalf("Could not init Kubernetes client! [%s]", err)
	}

	k := &KubeUtilInterface{
		Kclient:            client,
		ExcludedNamespaces: excludedNamespaces,
	}

	return k, nil
}

func envVarExists(key string) bool {
	_, exists := os.LookupEnv(key)
	return exists
}

// does a best guess to the location of your kubeconfig
func findKubeConfig() string {
	home := homedir.HomeDir()
	if envVarExists("KUBECONFIG") {
		kubeconfig := os.Getenv("KUBECONFIG")
		return kubeconfig
	}
	kubeconfig := fmt.Sprint(filepath.Join(home, ".kube", "config"))
	return kubeconfig
}

type LegacyInterfaceWrapper struct {
	*kubernetes.Clientset
}

func (f LegacyInterfaceWrapper) Secrets(namespace string) coreType.SecretInterface {
	return f.CoreV1().Secrets(namespace)
}

func (f LegacyInterfaceWrapper) Namespaces() coreType.NamespaceInterface {
	return f.CoreV1().Namespaces()
}

func (f LegacyInterfaceWrapper) ServiceAccounts(namespace string) coreType.ServiceAccountInterface {
	return f.CoreV1().ServiceAccounts(namespace)
}

func (f LegacyInterfaceWrapper) Core() coreType.CoreV1Interface {
	return f.CoreV1()
}

func newKubeClient() (KubeInterface, error) {
	var client *kubernetes.Clientset

	// we will automatically decide if this is running inside the cluster or on someones laptop
	// if the ENV vars KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT exist
	// then we can assume this app is running inside a k8s cluster
	if envVarExists("KUBERNETES_SERVICE_HOST") && envVarExists("KUBERNETES_SERVICE_PORT") {
		logrus.Info("Using InCluster k8s config")
		cfg, err := rest.InClusterConfig()

		if err != nil {
			return nil, err
		}

		client, err = kubernetes.NewForConfig(cfg)

		if err != nil {
			return nil, err
		}
	} else {
		logrus.Infof("using KUBECONFIG to determine your kubernetes connection")
		cfg, err := clientcmd.BuildConfigFromFlags("", findKubeConfig())

		if err != nil {
			logrus.Error("Got error trying to create client: ", err)
			return nil, err
		}

		client, err = kubernetes.NewForConfig(cfg)

		if err != nil {
			return nil, err
		}
	}

	return LegacyInterfaceWrapper{
		client,
	}, nil
}

// GetNamespaces returns all namespaces
func (k *KubeUtilInterface) GetNamespaces() (*v1.NamespaceList, error) {
	namespaces, err := k.Kclient.Namespaces().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		logrus.Error("Error getting namespaces: ", err)
		return nil, err
	}

	return namespaces, nil
}

// GetSecret get a secret
func (k *KubeUtilInterface) GetSecret(namespace, name string) (*v1.Secret, error) {
	secret, err := k.Kclient.Secrets(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		logrus.Error("Error getting secret: ", err)
		return nil, err
	}

	return secret, nil
}

// CreateSecret creates a secret
func (k *KubeUtilInterface) CreateSecret(namespace string, secret *v1.Secret) error {
	_, err := k.Kclient.Secrets(namespace).Create(context.TODO(), secret, metav1.CreateOptions{})

	if err != nil {
		logrus.Error("Error creating secret: ", err)
		return err
	}

	return nil
}

// UpdateSecret updates a secret
func (k *KubeUtilInterface) UpdateSecret(namespace string, secret *v1.Secret) error {
	_, err := k.Kclient.Secrets(namespace).Update(context.TODO(), secret, metav1.UpdateOptions{})

	if err != nil {
		logrus.Error("Error updating secret: ", err)
		return err
	}

	return nil
}

// GetServiceAccount updates a secret
func (k *KubeUtilInterface) GetServiceAccount(namespace, name string) (*v1.ServiceAccount, error) {
	sa, err := k.Kclient.ServiceAccounts(namespace).Get(context.TODO(), name, metav1.GetOptions{})

	if err != nil {
		logrus.Error("Error getting service account: ", err)
		return nil, err
	}

	return sa, nil
}

// UpdateServiceAccount updates a secret
func (k *KubeUtilInterface) UpdateServiceAccount(namespace string, sa *v1.ServiceAccount) error {
	_, err := k.Kclient.ServiceAccounts(namespace).Update(context.TODO(), sa, metav1.UpdateOptions{})

	if err != nil {
		logrus.Error("Error updating service account: ", err)
		return err
	}

	return nil
}

func (k *KubeUtilInterface) WatchNamespaces(resyncPeriod time.Duration, handler func(*v1.Namespace) error) {
	stopC := make(chan struct{})
	_, c := cache.NewInformer(
		// cache.NewListWatchFromClient(k.Kclient.Core().RESTClient(), "namespaces", v1.NamespaceAll, fields.Everything()),
		cache.NewListWatchFromClient(
			k.Kclient.Core().RESTClient(),
			"namespaces",
			v1.NamespaceAll,
			fields.Everything(),
		),
		&v1.Namespace{},
		resyncPeriod,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				if err := handler(obj.(*v1.Namespace)); err != nil {
					log.Println(err)
					os.Exit(1)
				}
			},
			UpdateFunc: func(_ interface{}, obj interface{}) {
				if err := handler(obj.(*v1.Namespace)); err != nil {
					log.Println(err)
					os.Exit(1)
				}
			},
		},
	)
	c.Run(stopC)
}
