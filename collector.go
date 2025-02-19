package main

import (
	"io/ioutil"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
	"github.com/vishvananda/netns"
)

const (
	NetnsPath         = "/run/netns/"
	InterfaceStatPath = "/sys/devices/virtual/net/"
	ProcStatPath      = "/proc/"

	collectorNamespace = "netns"
	collectorSubsystem = "network"
	netnsLabel         = "netns"
	deviceLabel        = "device"
	router             = "router"
	hostname           = "hostname"
	deviceIP           = "deviceIP"
)

type Collector struct {
	logger      logrus.FieldLogger
	config      *NetnsExporterConfig
	netnsStatus *prometheus.Desc
	intfStatus  *prometheus.Desc
	intfMetrics map[string]*prometheus.Desc
	procMetrics map[string]*PrometheusProcMetric
}

type PrometheusProcMetric struct {
	Config ProcMetric
	Desc   *prometheus.Desc
}

func NewCollector(config *NetnsExporterConfig, logger *logrus.Logger) *Collector {

	// Add descriptions for netns count
	netnsStatus := prometheus.NewDesc(
		prometheus.BuildFQName(collectorNamespace, collectorSubsystem, "namespace"),
		"Value is allways 1 for network namespace found",
		[]string{netnsLabel, hostname},
		nil,
	)

	// Add descriptions for interface adminStatus metric
	intfStatus := prometheus.NewDesc(
		prometheus.BuildFQName(collectorNamespace, collectorSubsystem, "up"),
		"Value is 1 if operstate is 'up', 0 otherwise.",
		[]string{netnsLabel, deviceLabel, router, hostname, deviceIP},
		nil,
	)

	// Add descriptions for interface metrics
	intfMetrics := make(map[string]*prometheus.Desc, len(config.InterfaceMetrics))
	for _, metric := range config.InterfaceMetrics {
		intfMetrics[metric] = prometheus.NewDesc(
			prometheus.BuildFQName(collectorNamespace, collectorSubsystem, metric+"_total"),
			"Interface statistics in the network namespace",
			[]string{netnsLabel, deviceLabel, router, hostname, deviceIP},
			nil,
		)
	}
	// Add descriptions for proc metrics
	procMetrics := make(map[string]*PrometheusProcMetric, len(config.InterfaceMetrics))
	for metricName, metric := range config.ProcMetrics {
		procMetrics[metricName] = &PrometheusProcMetric{
			Config: metric,
			Desc: prometheus.NewDesc(
				prometheus.BuildFQName(collectorNamespace, collectorSubsystem, metricName+"_total"),
				"Statistics from /proc filesystem in the network namespace",
				[]string{netnsLabel},
				nil,
			),
		}
	}

	return &Collector{
		logger:      logger.WithField("component", "collector"),
		config:      config,
		netnsStatus: netnsStatus,
		intfStatus:  intfStatus,
		intfMetrics: intfMetrics,
		procMetrics: procMetrics,
	}
}

func (c *Collector) Describe(ch chan<- *prometheus.Desc) {

	ch <- c.netnsStatus

	ch <- c.intfStatus

	for _, desc := range c.intfMetrics {
		ch <- desc
	}

	for _, metric := range c.procMetrics {
		ch <- metric.Desc
	}
}

func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	// Limit the number of concurrent goroutines, because each will block an entire
	// operating system thread. Maximum number of gorutines == number of CPU cores.
	wg := NewLimitedWaitGroup(c.config.Threads)
	startTime := time.Now()
	// Get namespases files
	nsFiles, err := ioutil.ReadDir(NetnsPath)
	if err != nil {
		c.logger.Errorf("Reading list of network nemaspaces failed: %s", err)

		return
	}

	// Filter namespaces by regexp if namespace-filters declared in config
	if (c.config.NamespacesFilter.BlacklistPattern != "") ||
		(c.config.NamespacesFilter.WhitelistPattern != "") {
		nsFiles = c.filterNsFiles(nsFiles)
	}

	c.logger.Debugf("Found %d namespaces", len(nsFiles))
	c.logger.Debugf("Only %d parallel goroutines will be run", runtime.NumCPU())

	// Get metrics from all of namespaces
	for _, ns := range nsFiles {
		wg.Add(1)

		go c.getMetricsFromNamespace(ns.Name(), wg, ch)
	}

	wg.Wait()
	c.logger.Debugf("collecting took %s for %d namespaces", time.Since(startTime), len(nsFiles))
}

func (c *Collector) getMetricsFromNamespace(namespace string, wg *LimitedWaitGroup, ch chan<- prometheus.Metric) {
	defer wg.Done()

	c.logger.Debugf("Start getting statistics for namespace %s", namespace)
	// Lock the OS Thread so we don't accidentally switch namespaces
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	startTime := time.Now()
	// Save current namespace
	curNs, err := netns.Get()
	if err != nil {
		c.logger.Errorf("Get current namespace %s failed: %s", namespace, err)

		return
	}
	defer curNs.Close()
	defer netns.Set(curNs) //nolint:errcheck

	// Switch namespace
	ns, err := netns.GetFromName(namespace)
	if err != nil {
		c.logger.Errorf("Get net namespace by name %s failed: %s", namespace, err)

		return
	}

	if err := netns.Set(ns); err != nil {
		c.logger.Errorf("Change net namespace to %s failed: %s", namespace, err)

		return
	}
	defer ns.Close()

	// Say to the kernel that we will use separate  context
	if err := syscall.Unshare(syscall.CLONE_NEWNS); err != nil { //nolint:typecheck
		c.logger.Errorf("Syscall unshare failed in namespace %s: %s", namespace, err)

		return
	}

	ch <- prometheus.MustNewConstMetric(c.netnsStatus, prometheus.CounterValue, 1, namespace, c.getHostname())

	// Don't let any mounts propagate back to the parent
	// See: https://github.com/shemminger/iproute2/blob/6754e1d9783458550dce8d309efb4091ec8089a5/lib/namespace.c#L77
	// and: https://www.kernel.org/doc/Documentation/filesystems/sharedsubtree.txt
	if err := syscall.Mount("", "/", "none", syscall.MS_SLAVE|syscall.MS_REC, ""); err != nil { //nolint:typecheck
		c.logger.Errorf("Mount root with rslave option failed in namepsace %s: %s", namespace, err)

		return
	}

	// Mount sysfs from net nemaspace
	if err := syscall.Mount(namespace, "/sys", "sysfs", 0, "ro"); err != nil { //nolint:typecheck
		c.logger.Errorf("Mount /sys from the namespace failed in namespace: %s", namespace, err)

		return
	}
	defer syscall.Unmount("/sys", syscall.MNT_DETACH) //nolint:errcheck,typecheck

	// Parse interfaces statistics
	ifFiles, err := ioutil.ReadDir(InterfaceStatPath)
	if err != nil {
		c.logger.Errorf("Reading sysfs directory for interface %s in namespace %s failed: %s", InterfaceStatPath, namespace, err)

		return
	}

	// Filter device name by regexp if device-filters declared in config
	if (c.config.DeviceFilter.BlacklistPattern != "") ||
		(c.config.DeviceFilter.WhitelistPattern != "") {
		ifFiles = c.filteriFFiles(ifFiles)
	}

	for _, ifFile := range ifFiles {
		// We don't need to get stat for lo interface
		if ifFile.Name() == "lo" {
			continue
		}

		c.logger.Debugf("Start getting statistics for interface %s in namespace %s", ifFile.Name(), namespace)

		// get ip address of device in namespace
		device_addr, err := c.getIPfromNS(namespace, ifFile.Name())
		if err != nil {
			c.logger.Errorf("Failed to get IP of device: %s", ifFile.Name)
		}

		// parse routerID from namespace
		routerID := strings.Replace(namespace, "qrouter-", "", -1)
		// get current hostname
		hostname := c.getHostname()

		value, _ := c.getDeviceStatusMetricfromNS(namespace, ifFile.Name())
		ch <- prometheus.MustNewConstMetric(c.intfStatus, prometheus.CounterValue, value, namespace, ifFile.Name(), routerID, hostname, device_addr)

		for metricName, desc := range c.intfMetrics {
			value := c.getMetricFromFile(namespace, InterfaceStatPath+ifFile.Name()+"/statistics/"+metricName)
			ch <- prometheus.MustNewConstMetric(desc, prometheus.CounterValue, value, namespace, ifFile.Name(), routerID, hostname, device_addr)
		}
	}

	// Parse of /proc statistics
	for _, metric := range c.procMetrics {
		value := c.getMetricFromFile(namespace, ProcStatPath+metric.Config.FileName)
		ch <- prometheus.MustNewConstMetric(metric.Desc, prometheus.CounterValue, value, namespace)
	}

	c.logger.Debugf("processing namespace %s took %s", namespace, time.Since(startTime))
}

func (c *Collector) getMetricFromFile(namespace, file string) float64 {
	data, err := ioutil.ReadFile(file)
	if err != nil {
		c.logger.Errorf("Error while reading statistic file %s in namespace %s: %s", file, namespace, err)

		return -1
	}

	stat, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
	if err != nil {
		c.logger.Printf("Error while parsing data from file %s in namespace %s: %s", file, namespace, err)

		return -1
	}

	return stat
}

func (c *Collector) filterNsFiles(nsFiles []os.FileInfo) []os.FileInfo {
	blacklistRegexp := c.config.NamespacesFilter.BlacklistRegexp
	whitelistRegexp := c.config.NamespacesFilter.WhitelistRegexp

	if blacklistRegexp.String() != "" {
		tmp := make([]os.FileInfo, 0)

		for _, ns := range nsFiles {
			if !blacklistRegexp.MatchString(ns.Name()) {
				tmp = append(tmp, ns)
			}
		}

		nsFiles = tmp
	}

	if whitelistRegexp.String() != "" {
		tmp := make([]os.FileInfo, 0)

		for _, ns := range nsFiles {
			if whitelistRegexp.MatchString(ns.Name()) {
				tmp = append(tmp, ns)
			}
		}

		nsFiles = tmp
	}

	return nsFiles
}

func (c *Collector) filteriFFiles(ifFiles []os.FileInfo) []os.FileInfo {
	blacklistRegexp := c.config.DeviceFilter.BlacklistRegexp
	whitelistRegexp := c.config.DeviceFilter.WhitelistRegexp

	if blacklistRegexp.String() != "" {
		tmp := make([]os.FileInfo, 0)

		for _, ifD := range ifFiles {
			if !blacklistRegexp.MatchString(ifD.Name()) {
				tmp = append(tmp, ifD)
			}
		}

		ifFiles = tmp
	}

	if whitelistRegexp.String() != "" {
		tmp := make([]os.FileInfo, 0)

		for _, ifD := range ifFiles {
			if whitelistRegexp.MatchString(ifD.Name()) {
				tmp = append(tmp, ifD)
			}
		}

		ifFiles = tmp
	}

	return ifFiles
}

func (c *Collector) getHostname() string {
	hostname, err := os.Hostname()
	if err != nil {
		c.logger.Debugf("Fail to get current hostname")
		return ""
	}
	return hostname
}

func (c *Collector) getIPfromNS(namespace, device string) (IP string, err error) {

	ns, err := netns.GetFromName(namespace)
	if err != nil {
		c.logger.Errorf("Failed to open namespace:", err)
		return "nil", err
	}
	defer ns.Close()

	netns.Set(ns)
	defer netns.Set(netns.None())

	// Get IP from specific ip adress
	iface, err := net.InterfaceByName(device)
	if err != nil {
		c.logger.Errorf("Failed to retrieve interfaces:", err)
		return "nil", err
	}
	addrs, err := iface.Addrs()
	if err != nil {
		c.logger.Errorf("Failed to retrieve addresses for interface", iface.Name, ":", err)
		return "nil", err
	}
	if len(addrs) > 0 {
		ip, _, err := net.ParseCIDR(addrs[0].String())
		if err != nil {
			c.logger.Errorf("Failed to parse IP address:", err)
			return "nil", err
		}
		c.logger.Debugf("IP address %s %s in namespace %s", iface.Name, ip, namespace)
		return ip.String(), nil

	}
	c.logger.Debugf("Interface %s no IP", iface.Name)
	return "", nil

}

func (c *Collector) getDeviceStatusMetricfromNS(namespace, device string) (adminStatus float64, err error) {

	ns, err := netns.GetFromName(namespace)
	if err != nil {
		c.logger.Errorf("Failed to open namespace:", err)
		return 2, err
	}
	defer ns.Close()

	netns.Set(ns)
	defer netns.Set(netns.None())

	iface, err := net.InterfaceByName(device)
	if err != nil {
		c.logger.Errorf("Failed to retrieve interfaces:", err)
		return 2, err
	}

	// Get adminStatus
	if iface.Flags&net.FlagUp != 0 {
		// "Up" -> 1
		return 1, nil
	} else {
		// "Down" -> 0
		return 0, nil
	}
}
