package origin

import (
	"net"
	"time"

	"github.com/golang/glog"

	"k8s.io/apimachinery/pkg/runtime/schema"
	utilwait "k8s.io/apimachinery/pkg/util/wait"
	kapi "k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/registry/core/service/allocator"
	etcdallocator "k8s.io/kubernetes/pkg/registry/core/service/allocator/storage"

	cmdutil "github.com/openshift/origin/pkg/cmd/util"
	"github.com/openshift/origin/pkg/dns"
	securitycontroller "github.com/openshift/origin/pkg/security/controller"
	"github.com/openshift/origin/pkg/security/mcs"
	"github.com/openshift/origin/pkg/security/uid"
	"github.com/openshift/origin/pkg/security/uidallocator"
)

// RunProjectAuthorizationCache starts the project authorization cache
func (c *MasterConfig) RunProjectAuthorizationCache() {
	// TODO: look at exposing a configuration option in future to control how often we run this loop
	period := 1 * time.Second
	c.ProjectAuthorizationCache.Run(period)
}

// RunAssetServer starts the asset server for the OpenShift UI.
func (c *MasterConfig) RunAssetServer() {
}

// RunDNSServer starts the DNS server
func (c *MasterConfig) RunDNSServer() {
	config, err := dns.NewServerDefaults()
	if err != nil {
		glog.Fatalf("Could not start DNS: %v", err)
	}
	switch c.Options.DNSConfig.BindNetwork {
	case "tcp":
		config.BindNetwork = "ip"
	case "tcp4":
		config.BindNetwork = "ipv4"
	case "tcp6":
		config.BindNetwork = "ipv6"
	}
	config.DnsAddr = c.Options.DNSConfig.BindAddress
	config.NoRec = !c.Options.DNSConfig.AllowRecursiveQueries

	_, port, err := net.SplitHostPort(c.Options.DNSConfig.BindAddress)
	if err != nil {
		glog.Fatalf("Could not start DNS: %v", err)
	}
	if port != "53" {
		glog.Warningf("Binding DNS on port %v instead of 53, which may not be resolvable from all clients", port)
	}

	if ok, err := cmdutil.TryListen(c.Options.DNSConfig.BindNetwork, c.Options.DNSConfig.BindAddress); !ok {
		glog.Warningf("Could not start DNS: %v", err)
		return
	}

	services, err := dns.NewCachedServiceAccessor(c.InternalKubeInformers.Core().InternalVersion().Services())
	if err != nil {
		glog.Fatalf("Could not start DNS: failed to add ClusterIP index: %v", err)
	}

	go func() {
		s := dns.NewServer(
			config,
			services,
			c.InternalKubeInformers.Core().InternalVersion().Endpoints().Lister(),
			"apiserver",
		)
		err = s.ListenAndServe()
		glog.Fatalf("Could not start DNS: %v", err)
	}()

	cmdutil.WaitForSuccessfulDial(false, "tcp", c.Options.DNSConfig.BindAddress, 100*time.Millisecond, 100*time.Millisecond, 100)

	glog.Infof("DNS listening at %s", c.Options.DNSConfig.BindAddress)
}

// RunProjectCache populates project cache, used by scheduler and project admission controller.
func (c *MasterConfig) RunProjectCache() {
	glog.Infof("Using default project node label selector: %s", c.Options.ProjectConfig.DefaultNodeSelector)
	go c.ProjectCache.Run(utilwait.NeverStop)
}

// RunSecurityAllocationController starts the security allocation controller process.
func (c *MasterConfig) RunSecurityAllocationController() {
	alloc := c.Options.ProjectConfig.SecurityAllocator
	if alloc == nil {
		glog.V(3).Infof("Security allocator is disabled - no UIDs assigned to projects")
		return
	}

	// TODO: move range initialization to run_config
	uidRange, err := uid.ParseRange(alloc.UIDAllocatorRange)
	if err != nil {
		glog.Fatalf("Unable to describe UID range: %v", err)
	}

	opts, err := c.RESTOptionsGetter.GetRESTOptions(schema.GroupResource{Resource: "securityuidranges"})
	if err != nil {
		glog.Fatalf("Unable to load storage options for security UID ranges")
	}

	var etcdAlloc *etcdallocator.Etcd
	uidAllocator := uidallocator.New(uidRange, func(max int, rangeSpec string) allocator.Interface {
		mem := allocator.NewContiguousAllocationMap(max, rangeSpec)
		etcdAlloc = etcdallocator.NewEtcd(mem, "/ranges/uids", kapi.Resource("uidallocation"), opts.StorageConfig)
		return etcdAlloc
	})
	mcsRange, err := mcs.ParseRange(alloc.MCSAllocatorRange)
	if err != nil {
		glog.Fatalf("Unable to describe MCS category range: %v", err)
	}

	kclient := c.SecurityAllocationControllerClient()

	repair := securitycontroller.NewRepair(time.Minute, kclient.Core().Namespaces(), uidRange, etcdAlloc)
	if err := repair.RunOnce(); err != nil {
		// TODO: v scary, may need to use direct etcd calls?
		// If the security controller fails during RunOnce it could mean a
		// couple of things:
		// 1. an unexpected etcd error occurred getting an allocator or the namespaces
		// 2. the allocation blocks were full - would result in an admission controller that is unable
		//	  to create the strategies correctly which would likely mean that the cluster
		//	  would not admit pods the the majority of users.
		// 3. an unexpected error persisting an allocation for a namespace has occurred - same as above
		// In all cases we do not want to continue normal operations, this should be fatal.
		glog.Fatalf("Unable to initialize namespaces: %v", err)
	}

	controller := securitycontroller.NewNamespaceSecurityDefaultsController(
		c.ExternalKubeInformers.Core().V1().Namespaces(),
		kclient.Core().Namespaces(),
		uidAllocator,
		securitycontroller.DefaultMCSAllocation(uidRange, mcsRange, alloc.MCSLabelsPerProject),
	)
	// TODO: scale out
	go controller.Run(utilwait.NeverStop, 1)
}

// RunGroupCache starts the group cache
func (c *MasterConfig) RunGroupCache() {
	c.GroupCache.Run()
}
