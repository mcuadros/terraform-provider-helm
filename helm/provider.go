package helm

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/terraform/helper/pathorcontents"
	"github.com/hashicorp/terraform/helper/resource"
	"github.com/hashicorp/terraform/helper/schema"
	"github.com/hashicorp/terraform/terraform"
	homedir "github.com/mitchellh/go-homedir"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	// Import to initialize client auth plugins.
	"k8s.io/cli-runtime/pkg/genericclioptions"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/helm/cmd/helm/installer"
	"k8s.io/helm/pkg/helm"
	helm_env "k8s.io/helm/pkg/helm/environment"
	"k8s.io/helm/pkg/helm/helmpath"
	"k8s.io/helm/pkg/helm/portforwarder"
	"k8s.io/helm/pkg/kube"
	"k8s.io/helm/pkg/proto/hapi/services"
	"k8s.io/helm/pkg/storage"
	"k8s.io/helm/pkg/storage/driver"
	"k8s.io/helm/pkg/tiller"
	tiller_env "k8s.io/helm/pkg/tiller/environment"
	"k8s.io/helm/pkg/tlsutil"
	"k8s.io/helm/pkg/version"
)

// Provider returns the provider schema to Terraform.
func Provider() terraform.ResourceProvider {
	return &schema.Provider{
		Schema: map[string]*schema.Schema{
			"host": {
				Type:        schema.TypeString,
				Required:    true,
				DefaultFunc: schema.EnvDefaultFunc(helm_env.HostEnvVar, ""),
				Description: "Set an alternative Tiller host. The format is host:port.",
			},
			"home": {
				Type:        schema.TypeString,
				Required:    true,
				DefaultFunc: schema.EnvDefaultFunc(helm_env.HomeEnvVar, helm_env.DefaultHelmHome),
				Description: "Set an alternative location for Helm files. By default, these are stored in '~/.helm'.",
			},
			"namespace": {
				Type:        schema.TypeString,
				Optional:    true,
				Default:     tiller_env.DefaultTillerNamespace,
				Description: "Set an alternative Tiller namespace.",
			},
			"init_helm_home": {
				Type:        schema.TypeBool,
				Optional:    true,
				Default:     true,
				Description: "Initialize Helm home directory if it is not already initialized, defaults to true.",
			},
			"install_tiller": {
				Type:        schema.TypeBool,
				Optional:    true,
				Default:     true,
				Description: "Install Tiller if it is not already installed.",
			},
			"tillerless": {
				Type:        schema.TypeBool,
				Optional:    true,
				Default:     false,
				Description: "Enable tillerless mode and run Tiller locally.",
			},
			"tiller_image": {
				Type:        schema.TypeString,
				Optional:    true,
				Default:     "gcr.io/kubernetes-helm/tiller:v2.14.3",
				Description: "Tiller image to install.",
			},
			"service_account": {
				Type:        schema.TypeString,
				Optional:    true,
				Default:     "default",
				Description: "Service account to install Tiller with.",
			},
			"automount_service_account_token": {
				Type:        schema.TypeBool,
				Optional:    true,
				Default:     true,
				Description: "Auto-mount the given service account to tiller.",
			},
			"override": {
				Type:        schema.TypeList,
				Optional:    true,
				Description: "Override values for the Tiller Deployment manifest.",
				Elem:        &schema.Schema{Type: schema.TypeString},
			},
			"max_history": {
				Type:        schema.TypeInt,
				Optional:    true,
				Default:     0,
				Description: "Maximum number of release versions stored per release.",
			},
			"debug": {
				Type:        schema.TypeBool,
				Optional:    true,
				Description: "Debug indicates whether or not Helm is running in Debug mode.",
			},
			"plugins_disable": {
				Type:        schema.TypeBool,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc(helm_env.PluginDisableEnvVar, "true"),
				Description: "Disable plugins. Set HELM_NO_PLUGINS=0 to enable plugins.",
			},
			"insecure": {
				Type:        schema.TypeBool,
				Optional:    true,
				Description: "Whether server should be accessed without verifying the TLS certificate.",
			},
			"enable_tls": {
				Type:        schema.TypeBool,
				Optional:    true,
				Description: "Enables TLS communications with the Tiller.",
			},
			"client_key": {
				Type:        schema.TypeString,
				Optional:    true,
				Description: "PEM-encoded client certificate key for TLS authentication.",
			},
			"client_certificate": {
				Type:        schema.TypeString,
				Optional:    true,
				Description: "PEM-encoded client certificate for TLS authentication.",
			},
			"ca_certificate": {
				Type:        schema.TypeString,
				Optional:    true,
				Description: "PEM-encoded root certificates bundle for TLS authentication.",
			},
			"tillerless_storage": {
				Type:        schema.TypeString,
				Optional:    true,
				Default:     "secret",
				Description: "storage driver to use. One of 'configmap' or 'secret'. Default to \"secret\".",
			},
			"tillerless_tls_key": {
				Type:        schema.TypeString,
				Optional:    true,
				Description: "PEM-encoded client certificate key for TLS authentication.",
			},
			"tillerless_tls_certificate": {
				Type:        schema.TypeString,
				Optional:    true,
				Description: "PEM-encoded client certificate for TLS authentication.",
			},
			"tillerless_tls_ca_certificate": {
				Type:        schema.TypeString,
				Optional:    true,
				Description: "PEM-encoded root certificates bundle for TLS authentication.",
			},
			"kubernetes": {
				Type:        schema.TypeList,
				MaxItems:    1,
				Optional:    true,
				Description: "Kubernetes configuration.",
				Elem:        kubernetesResource(),
			},
		},
		ResourcesMap: map[string]*schema.Resource{
			"helm_release":    resourceRelease(),
			"helm_repository": resourceRepository(),
		},
		DataSourcesMap: map[string]*schema.Resource{
			"helm_repository": dataRepository(),
		},
		ConfigureFunc: providerConfigure,
	}
}

func kubernetesResource() *schema.Resource {
	return &schema.Resource{
		Schema: map[string]*schema.Schema{
			"host": {
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("KUBE_HOST", ""),
				Description: "The hostname (in form of URI) of Kubernetes master. Can be sourced from `KUBE_HOST`.",
			},
			"username": {
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("KUBE_USER", ""),
				Description: "The username to use for HTTP basic authentication when accessing the Kubernetes master endpoint. Can be sourced from `KUBE_USER`.",
			},
			"password": {
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("KUBE_PASSWORD", ""),
				Description: "The password to use for HTTP basic authentication when accessing the Kubernetes master endpoint. Can be sourced from `KUBE_PASSWORD`.",
			},
			"token": {
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("KUBE_BEARER_TOKEN", ""),
				Description: "The bearer token to use for authentication when accessing the Kubernetes master endpoint. Can be sourced from `KUBE_BEARER_TOKEN`.",
			},
			"insecure": {
				Type:        schema.TypeBool,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("KUBE_INSECURE", false),
				Description: "Whether server should be accessed without verifying the TLS certificate. Can be sourced from `KUBE_INSECURE`.",
			},
			"client_certificate": {
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("KUBE_CLIENT_CERT_DATA", ""),
				Description: "PEM-encoded client certificate for TLS authentication. Can be sourced from `KUBE_CLIENT_CERT_DATA`.",
			},
			"client_key": {
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("KUBE_CLIENT_KEY_DATA", ""),
				Description: "PEM-encoded client certificate key for TLS authentication. Can be sourced from `KUBE_CLIENT_KEY_DATA`.",
			},
			"cluster_ca_certificate": {
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("KUBE_CLUSTER_CA_CERT_DATA", ""),
				Description: "PEM-encoded root certificates bundle for TLS authentication. Can be sourced from `KUBE_CLUSTER_CA_CERT_DATA`.",
			},
			"config_path": {
				Type:     schema.TypeString,
				Optional: true,
				DefaultFunc: schema.MultiEnvDefaultFunc(
					[]string{
						"KUBE_CONFIG",
						"KUBECONFIG",
					},
					"~/.kube/config"),
				Description: "Path to the kube config file, defaults to ~/.kube/config. Can be sourced from `KUBE_CONFIG`.",
			},
			"config_context": {
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("KUBE_CTX", ""),
				Description: "Context to choose from the config file. Can be sourced from `KUBE_CTX`.",
			},
			"in_cluster": {
				Type:        schema.TypeBool,
				Optional:    true,
				Description: "Retrieve config from Kubernetes cluster.",
			},
			"load_config_file": {
				Type:        schema.TypeBool,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("KUBE_LOAD_CONFIG_FILE", true),
				Description: "By default the local config (~/.kube/config) is loaded when you use this provider. This option at false disable this behaviour.",
			},
		},
	}
}

func providerConfigure(d *schema.ResourceData) (interface{}, error) {
	return NewMeta(d)
}

// Meta is the meta information structure for the provider
type Meta struct {
	Settings         *helm_env.EnvSettings
	TLSConfig        *tls.Config
	K8sClient        kubernetes.Interface
	K8sConfig        *rest.Config
	Tunnel           *kube.Tunnel
	RootServer       *grpc.Server
	TillerEnv        *tiller_env.Environment
	TillerK8sClient  *kube.Client
	TillerK8sConfig  *genericclioptions.ConfigFlags
	DefaultNamespace string

	data *schema.ResourceData

	// Mutex used for lock the Tiller installation and Tunnel creation.
	sync.Mutex
}

// NewMeta will construct a new Meta from the provided ResourceData
func NewMeta(d *schema.ResourceData) (*Meta, error) {
	m := &Meta{data: d}
	m.buildSettings(m.data)

	if err := m.buildTLSConfig(m.data); err != nil {
		return nil, err
	}

	if err := m.buildK8sClient(m.data); err != nil {
		return nil, err
	}

	if err := m.buildTillerK8sClient(m.data); err != nil {
		return nil, err
	}

	if err := m.initHelmHomeIfNeeded(m.data); err != nil {
		return nil, err
	}

	m.TillerEnv = tiller_env.New()

	return m, nil
}

func (m *Meta) buildSettings(d *schema.ResourceData) {
	m.Settings = &helm_env.EnvSettings{
		Home:            helmpath.Home(d.Get("home").(string)),
		TillerHost:      d.Get("host").(string),
		TillerNamespace: d.Get("namespace").(string),
		Debug:           d.Get("debug").(bool),
	}
}

func (m *Meta) buildK8sClient(d *schema.ResourceData) error {
	_, hasStatic := d.GetOk("kubernetes")

	c, err := getK8sConfig(d)
	if err != nil {
		debug("could not get Kubernetes config: %s", err)
		if !hasStatic {
			return err
		}
	}

	cfg, err := c.ClientConfig()
	if err != nil {
		debug("could not get Kubernetes client config: %s", err)
		if !hasStatic {
			return err
		}
	}

	if cfg == nil {
		cfg = &rest.Config{}
	}

	// Overriding with static configuration
	cfg.UserAgent = fmt.Sprintf("HashiCorp/1.0 Terraform/%s", terraform.VersionString())

	if v, ok := k8sGetOk(d, "host"); ok {
		cfg.Host = v.(string)
	}
	if v, ok := k8sGetOk(d, "username"); ok {
		cfg.Username = v.(string)
	}
	if v, ok := k8sGetOk(d, "password"); ok {
		cfg.Password = v.(string)
	}
	if v, ok := k8sGetOk(d, "token"); ok {
		cfg.BearerToken = v.(string)
	}
	if v, ok := k8sGetOk(d, "insecure"); ok {
		cfg.Insecure = v.(bool)
	}
	if v, ok := k8sGetOk(d, "cluster_ca_certificate"); ok {
		cfg.CAData = []byte(v.(string))
	}
	if v, ok := k8sGetOk(d, "client_certificate"); ok {
		cfg.CertData = []byte(v.(string))
	}
	if v, ok := k8sGetOk(d, "client_key"); ok {
		cfg.KeyData = []byte(v.(string))
	}

	m.K8sConfig = cfg
	m.K8sClient, err = kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("failed to configure kubernetes config: %s", err)
	}

	return nil
}

func (m *Meta) buildTillerK8sClient(d *schema.ResourceData) error {
	tillerK8sConfig := genericclioptions.NewConfigFlags(true)

	if !k8sGet(d, "in_cluster").(bool) && k8sGet(d, "load_config_file").(bool) {
		explicitPath, err := homedir.Expand(k8sGet(d, "config_path").(string))
		if err != nil {
			return err
		}

		context := k8sGet(d, "config_context").(string)
		if context != "" {
			*tillerK8sConfig.Context = context
		}

		*tillerK8sConfig.KubeConfig = explicitPath
	}

	if v, ok := k8sGetOk(d, "host"); ok {
		*tillerK8sConfig.APIServer = v.(string)
	}
	if v, ok := k8sGetOk(d, "username"); ok {
		*tillerK8sConfig.Username = v.(string)
	}
	if v, ok := k8sGetOk(d, "password"); ok {
		*tillerK8sConfig.Password = v.(string)
	}
	if v, ok := k8sGetOk(d, "token"); ok {
		*tillerK8sConfig.BearerToken = v.(string)
	}
	if v, ok := k8sGetOk(d, "insecure"); ok {
		*tillerK8sConfig.Insecure = v.(bool)
	}
	if v, ok := k8sGetOk(d, "cluster_ca_certificate"); ok {
		caFile, err := writeTempFile([]byte(v.(string)))
		if err != nil {
			return err
		}
		*tillerK8sConfig.CAFile = caFile
	}

	m.TillerK8sConfig = tillerK8sConfig
	m.TillerK8sClient = kube.New(tillerK8sConfig)

	return nil
}

var k8sPrefix = "kubernetes.0."

func k8sGetOk(d *schema.ResourceData, key string) (interface{}, bool) {
	value, ok := d.GetOk(k8sPrefix + key)

	// fix: DefaultFunc is not being triggerred on TypeList
	schema := kubernetesResource().Schema[key]
	if !ok && schema.DefaultFunc != nil {
		value, _ = schema.DefaultFunc()

		switch v := value.(type) {
		case string:
			ok = len(v) != 0
		case bool:
			ok = v
		}
	}

	return value, ok
}

func k8sGet(d *schema.ResourceData, key string) interface{} {
	value, _ := k8sGetOk(d, key)
	return value
}

func getK8sConfig(d *schema.ResourceData) (clientcmd.ClientConfig, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{}

	if !k8sGet(d, "in_cluster").(bool) && k8sGet(d, "load_config_file").(bool) {
		explicitPath, err := homedir.Expand(k8sGet(d, "config_path").(string))
		if err != nil {
			return nil, err
		}

		rules.ExplicitPath = explicitPath
		rules.DefaultClientConfig = &clientcmd.DefaultClientConfig

		context := k8sGet(d, "config_context").(string)
		if context != "" {
			overrides.CurrentContext = context
		}
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides), nil
}

// GetHelmClient will return a new Helm client
func (m *Meta) GetHelmClient() (helm.Interface, error) {
	if err := m.initialize(); err != nil {
		return nil, err
	}

	return m.buildHelmClient(), nil
}

func (m *Meta) initialize() error {
	m.Lock()
	defer m.Unlock()

	if err := m.runLocalTillerIfNeeded(m.data); err != nil {
		return err
	}

	if err := m.installTillerIfNeeded(m.data); err != nil {
		return err
	}

	if err := m.buildTunnel(m.data); err != nil {
		return err
	}

	return nil
}

func (m *Meta) initHelmHomeIfNeeded(d *schema.ResourceData) error {
	if !d.Get("init_helm_home").(bool) {
		return nil
	}

	stableRepositoryURL := "https://kubernetes-charts.storage.googleapis.com"
	localRepositoryURL := "http://127.0.0.1:8879/charts"

	if err := installer.Initialize(m.Settings.Home, os.Stdout, false, *m.Settings, stableRepositoryURL, localRepositoryURL); err != nil {
		return fmt.Errorf("error initializing local helm home: %s", err)
	}
	return nil
}

func (m *Meta) installTillerIfNeeded(d *schema.ResourceData) error {
	if !d.Get("install_tiller").(bool) || d.Get("tillerless").(bool) {
		return nil
	}

	o := &installer.Options{}
	o.Namespace = d.Get("namespace").(string)
	o.ImageSpec = d.Get("tiller_image").(string)
	o.ServiceAccount = d.Get("service_account").(string)
	o.AutoMountServiceAccountToken = d.Get("automount_service_account_token").(bool)
	o.MaxHistory = d.Get("max_history").(int)

	for _, rule := range d.Get("override").([]interface{}) {
		o.Values = append(o.Values, rule.(string))
	}

	o.EnableTLS = d.Get("enable_tls").(bool)
	if o.EnableTLS {
		o.TLSCertFile = d.Get("client_certificate").(string)
		o.TLSKeyFile = d.Get("client_key").(string)
		o.VerifyTLS = !d.Get("insecure").(bool)
		if o.VerifyTLS {
			o.TLSCaCertFile = d.Get("ca_certificate").(string)
		}
	}

	if err := installer.Install(m.K8sClient, o); err != nil {
		if errors.IsAlreadyExists(err) {
			return nil
		}

		return fmt.Errorf("error installing: %s", err)
	}

	if err := m.waitForTiller(o); err != nil {
		return err
	}

	debug("Tiller has been installed into your Kubernetes Cluster.")
	return nil
}

func (m *Meta) waitForTiller(o *installer.Options) error {
	const deployment = "tiller-deploy"
	stateConf := &resource.StateChangeConf{
		Target:  []string{"Running"},
		Pending: []string{"Pending"},
		Timeout: 5 * time.Minute,
		Refresh: func() (interface{}, string, error) {
			debug("Waiting for tiller-deploy to become available.")
			obj, err := m.K8sClient.AppsV1().Deployments(o.Namespace).Get(deployment, metav1.GetOptions{})
			if err != nil {
				return obj, "Error", err
			}

			if obj.Status.ReadyReplicas > 0 {
				return obj, "Running", nil
			}

			return obj, "Pending", nil
		},
	}

	_, err := stateConf.WaitForState()
	return err
}

func (m *Meta) buildTunnel(d *schema.ResourceData) error {
	if m.Settings.TillerHost != "" {
		return nil
	}

	// Wait a reasonable time for tiller, even if we didn't deploy it this run
	o := &installer.Options{}
	o.Namespace = m.Settings.TillerNamespace
	if err := m.waitForTiller(o); err != nil {
		return err
	}

	var err error
	m.Tunnel, err = portforwarder.New(m.Settings.TillerNamespace, m.K8sClient, m.K8sConfig)
	if err != nil {
		return fmt.Errorf("error creating tunnel: %q", err)
	}

	m.Settings.TillerHost = fmt.Sprintf("127.0.0.1:%d", m.Tunnel.Local)
	debug("Created tunnel using local port: '%d'\n", m.Tunnel.Local)
	return nil
}

func (m *Meta) buildHelmClient() helm.Interface {
	options := []helm.Option{
		helm.Host(m.Settings.TillerHost),
	}

	if m.TLSConfig != nil {
		debug("Found TLS settings: configuring helm client with TLS")
		options = append(options, helm.WithTLS(m.TLSConfig))
	}

	return helm.NewClient(options...)
}

func (m *Meta) buildTLSConfig(d *schema.ResourceData) error {
	// Don't initialize TLSConfig if TLS is disabled
	if !d.Get("enable_tls").(bool) {
		return nil
	}

	// The default uses the files in the provider configured helm home
	helmHome := d.Get("home").(string)
	clientKeyDefault := filepath.Join(helmHome, "key.pem")
	clientCertDefault := filepath.Join(helmHome, "cert.pem")
	caCertDefault := filepath.Join(helmHome, "ca.pem")

	keyPEMBlock, err := getContent(d, "client_key", clientKeyDefault)
	if err != nil {
		return err
	}
	certPEMBlock, err := getContent(d, "client_certificate", clientCertDefault)
	if err != nil {
		return err
	}
	if len(keyPEMBlock) == 0 && len(certPEMBlock) == 0 {
		return nil
	}

	cfg := &tls.Config{
		InsecureSkipVerify: d.Get("insecure").(bool),
	}

	cert, err := tls.X509KeyPair(certPEMBlock, keyPEMBlock)
	if err != nil {
		return fmt.Errorf("could not read x509 key pair: %s", err)
	}

	cfg.Certificates = []tls.Certificate{cert}

	caPEMBlock, err := getContent(d, "ca_certificate", caCertDefault)
	if err != nil {
		return err
	}

	if !cfg.InsecureSkipVerify && len(caPEMBlock) != 0 {
		cfg.RootCAs = x509.NewCertPool()
		if !cfg.RootCAs.AppendCertsFromPEM(caPEMBlock) {
			return fmt.Errorf("failed to parse ca_certificate")
		}
	}

	m.TLSConfig = cfg
	return nil
}

func getContent(d *schema.ResourceData, key, def string) ([]byte, error) {
	// Check if the key is defined. If not, use the default.
	filename := d.Get(key).(string)
	if filename == "" {
		filename = def
	}
	debug("TLS settings: Attempting to read contents of %s from %s", key, filename)

	content, _, err := pathorcontents.Read(filename)
	if err != nil {
		return nil, err
	}

	if content == def {
		return nil, nil
	}

	return []byte(content), nil
}

func debug(format string, a ...interface{}) {
	log.Printf("[DEBUG] %s", fmt.Sprintf(format, a...))
}

func (m *Meta) runLocalTillerIfNeeded(d *schema.ResourceData) error {
	if !d.Get("tillerless").(bool) {
		return nil
	}

	defer m.cleanTempFiles()

	var (
		store                   = d.Get("tillerless_storage").(string)
		tillerlessTlsKeyFile    = d.Get("tillerless_tls_key").(string)
		tillerlessTlsCertFile   = d.Get("tillerless_tls_certificate").(string)
		tillerlessTlsCaCertFile = d.Get("tillerless_tls_ca_certificate").(string)
		maxHistory              = d.Get("max_history").(int)
	)

	switch store {
	case "configmap":
		cfgmaps := driver.NewConfigMaps(m.K8sClient.CoreV1().ConfigMaps(m.Settings.TillerNamespace))
		m.TillerEnv.Releases = storage.Init(cfgmaps)
	case "secret":
		secrets := driver.NewSecrets(m.K8sClient.CoreV1().Secrets(m.Settings.TillerNamespace))
		m.TillerEnv.Releases = storage.Init(secrets)
	}

	if maxHistory > 0 {
		m.TillerEnv.Releases.MaxHistory = maxHistory
	}

	m.TillerEnv.KubeClient = m.TillerK8sClient

	if tlsEnable || tlsVerify {
		opts := tlsutil.Options{CertFile: tillerlessTlsCertFile, KeyFile: tillerlessTlsKeyFile}
		if tlsVerify {
			opts.CaCertFile = tillerlessTlsCaCertFile
		}
	}

	var opts []grpc.ServerOption
	if tlsEnable || tlsVerify {
		tlsOptions := tlsutil.Options{CertFile: tillerlessTlsCertFile, KeyFile: tillerlessTlsKeyFile}
		if tlsVerify {
			tlsOptions.CaCertFile = tillerlessTlsCaCertFile

			// We want to force the client to not only provide a cert, but to
			// provide a cert that we can validate.
			// http://www.bite-code.com/2015/06/25/tls-mutual-auth-in-golang/
			tlsOptions.ClientAuth = tls.RequireAndVerifyClientCert
		}
		cfg, err := tlsutil.ServerConfig(tlsOptions)
		if err != nil {
			return fmt.Errorf("Could not create server TLS configuration: %v", err)
		}
		opts = append(opts, grpc.Creds(credentials.NewTLS(cfg)))
	}

	opts = append(opts, grpc.KeepaliveParams(keepalive.ServerParameters{
		MaxConnectionIdle: 10 * time.Minute,
		// If needed, we can configure the max connection age
	}))

	opts = append(opts, grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
		MinTime: time.Duration(20) * time.Second, // For compatibility with the client keepalive.ClientParameters
	}))

	m.RootServer = tiller.NewServer(opts...)

	lstn, err := net.Listen("tcp", m.Settings.TillerHost)
	if err != nil {
		if strings.Contains(err.Error(), "address already in use") {
			debug("Tiller host %s is already used", m.Settings.TillerHost)
			return nil
		}
		return fmt.Errorf("Server died: %s", err)
	}

	debug("Starting local Tiller %s (tls=%t)", version.GetVersion(), tlsEnable || tlsVerify)
	debug("GRPC listening on %s", m.Settings.TillerHost)
	debug("Storage driver is %s", m.TillerEnv.Releases.Name())
	debug("Max history per release is %d", maxHistory)

	srvErrCh := make(chan error)
	go func() {
		defer close(srvErrCh)

		svc := tiller.NewReleaseServer(m.TillerEnv, m.K8sClient, false)
		services.RegisterReleaseServiceServer(m.RootServer, svc)
		if err := m.RootServer.Serve(lstn); err != nil {
			srvErrCh <- err
			return
		}
	}()

	debug("Tiller has been started locally.")
	return nil
}

func writeTempFile(content []byte) (string, error) {
	tmpfile, err := ioutil.TempFile("", "helm-*")
	if err != nil {
		return "", err
	}

	if _, err := tmpfile.Write(content); err != nil {
		return "", err
	}
	if err := tmpfile.Close(); err != nil {
		return "", err
	}

	return tmpfile.Name(), nil
}

func (m *Meta) cleanTempFiles() {
	os.Remove(*m.TillerK8sConfig.CAFile)
}

var (
	tlsCaCertFile string // path to TLS CA certificate file
	tlsCertFile   string // path to TLS certificate file
	tlsKeyFile    string // path to TLS key file
	tlsVerify     bool   // enable TLS and verify remote certificates
	tlsEnable     bool   // enable TLS
)
