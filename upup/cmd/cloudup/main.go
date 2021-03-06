package main

import (
	"flag"
	"fmt"
	"github.com/golang/glog"
	"io/ioutil"
	"k8s.io/kube-deploy/upup/pkg/fi"
	"k8s.io/kube-deploy/upup/pkg/fi/cloudup"
	"k8s.io/kube-deploy/upup/pkg/fi/cloudup/awstasks"
	"k8s.io/kube-deploy/upup/pkg/fi/cloudup/awsup"
	"k8s.io/kube-deploy/upup/pkg/fi/cloudup/gce"
	"k8s.io/kube-deploy/upup/pkg/fi/cloudup/gcetasks"
	"k8s.io/kube-deploy/upup/pkg/fi/cloudup/terraform"
	"k8s.io/kube-deploy/upup/pkg/fi/fitasks"
	"k8s.io/kube-deploy/upup/pkg/fi/loader"
	"k8s.io/kube-deploy/upup/pkg/fi/utils"
	"k8s.io/kube-deploy/upup/pkg/fi/vfs"
	"os"
	"path"
	"strings"
)

func main() {
	dryrun := false
	flag.BoolVar(&dryrun, "dryrun", false, "Don't create cloud resources; just show what would be done")
	target := "direct"
	flag.StringVar(&target, "target", target, "Target - direct, terraform")
	configFile := ""
	flag.StringVar(&configFile, "conf", configFile, "Configuration file to load")
	modelDirs := "models/proto,models/cloudup"
	flag.StringVar(&modelDirs, "model", modelDirs, "Source directory to use as model (separate multiple models with commas)")
	stateLocation := "./state"
	flag.StringVar(&stateLocation, "state", stateLocation, "Location to use to store configuration state")
	nodeModelDir := "models/nodeup"
	flag.StringVar(&nodeModelDir, "nodemodel", nodeModelDir, "Source directory to use as model for node configuration")

	// TODO: Replace all these with a direct binding to the CloudConfig
	// (we have plenty of reflection helpers if one isn't already available!)
	config := &cloudup.CloudConfig{}

	flag.StringVar(&config.CloudProvider, "cloud", config.CloudProvider, "Cloud provider to use - gce, aws")

	zones := ""
	flag.StringVar(&zones, "zones", "", "Zones in which to run nodes")
	masterZones := ""
	flag.StringVar(&zones, "master-zones", masterZones, "Zones in which to run masters (must be an odd number)")

	flag.StringVar(&config.Project, "project", config.Project, "Project to use (must be set on GCE)")
	flag.StringVar(&config.ClusterName, "name", config.ClusterName, "Name for cluster")
	flag.StringVar(&config.KubernetesVersion, "kubernetes-version", config.KubernetesVersion, "Version of kubernetes to run (defaults to latest)")
	//flag.StringVar(&config.Region, "region", config.Region, "Cloud region to target")

	sshPublicKey := path.Join(os.Getenv("HOME"), ".ssh", "id_rsa.pub")
	flag.StringVar(&sshPublicKey, "ssh-public-key", sshPublicKey, "SSH public key to use")

	nodeSize := ""
	flag.StringVar(&nodeSize, "node-size", nodeSize, "Set instance size for nodes")

	masterSize := ""
	flag.StringVar(&masterSize, "master-size", masterSize, "Set instance size for masters")

	nodeCount := 0
	flag.IntVar(&nodeCount, "node-count", nodeCount, "Set the number of nodes")

	dnsZone := ""
	flag.StringVar(&dnsZone, "dns-zone", dnsZone, "DNS hosted zone to use (defaults to last two components of cluster name)")

	flag.Parse()

	config.NodeZones = parseZoneList(zones)
	if masterZones == "" {
		config.MasterZones = config.NodeZones
	} else {
		config.MasterZones = parseZoneList(masterZones)
	}

	if nodeSize != "" {
		config.NodeMachineType = nodeSize
	}
	if nodeCount != 0 {
		config.NodeCount = nodeCount
	}

	if masterSize != "" {
		config.MasterMachineType = masterSize
	}

	if dnsZone != "" {
		config.DNSZone = dnsZone
	}

	if dryrun {
		target = "dryrun"
	}

	statePath := vfs.NewFSPath(stateLocation)
	workDir := stateLocation

	stateStore, err := fi.NewVFSStateStore(statePath)
	if err != nil {
		glog.Errorf("error building state store: %v", err)
		os.Exit(1)
	}

	cmd := &CreateClusterCmd{
		Config:       config,
		ModelDirs:    strings.Split(modelDirs, ","),
		StateStore:   stateStore,
		Target:       target,
		NodeModelDir: nodeModelDir,
		SSHPublicKey: sshPublicKey,
		WorkDir:      workDir,
	}

	if configFile != "" {
		//confFile := path.Join(cmd.StateDir, "kubernetes.yaml")
		err := cmd.LoadConfig(configFile)
		if err != nil {
			glog.Errorf("error loading config: %v", err)
			os.Exit(1)
		}
	}

	err = cmd.Run()
	if err != nil {
		glog.Errorf("error running command: %v", err)
		os.Exit(1)
	}

	glog.Infof("Completed successfully")
}

func parseZoneList(s string) []string {
	var filtered []string
	for _, v := range strings.Split(s, ",") {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		v = strings.ToLower(v)
		filtered = append(filtered, v)
	}
	return filtered
}

type CreateClusterCmd struct {
	// Config is the cluster configuration
	Config *cloudup.CloudConfig
	// ModelDir is a list of directories in which the cloudup model are found
	ModelDirs []string
	// StateStore is a StateStore in which we store state (such as the PKI tree)
	StateStore fi.StateStore
	// Target specifies how we are operating e.g. direct to GCE, or AWS, or dry-run, or terraform
	Target string
	// The directory in which the node model is found
	NodeModelDir string
	// The SSH public key (file) to use
	SSHPublicKey string
	// WorkDir is a local directory in which we place output, can cache files etc
	WorkDir string
}

func (c *CreateClusterCmd) LoadConfig(configFile string) error {
	conf, err := ioutil.ReadFile(configFile)
	if err != nil {
		return fmt.Errorf("error loading configuration file %q: %v", configFile, err)
	}
	err = utils.YamlUnmarshal(conf, c.Config)
	if err != nil {
		return fmt.Errorf("error parsing configuration file %q: %v", configFile, err)
	}
	return nil
}

func (c *CreateClusterCmd) Run() error {
	// TODO: Make these configurable?
	useMasterASG := true
	useMasterLB := false

	// We (currently) have to use protokube with ASGs
	useProtokube := useMasterASG

	if c.Config.ClusterName == "" {
		return fmt.Errorf("-name is required (e.g. mycluster.myzone.com)")
	}

	if c.Config.MasterPublicName == "" {
		c.Config.MasterPublicName = "api." + c.Config.ClusterName
	}
	if c.Config.DNSZone == "" {
		tokens := strings.Split(c.Config.MasterPublicName, ".")
		c.Config.DNSZone = strings.Join(tokens[len(tokens)-2:], ".")
		glog.Infof("Defaulting DNS zone to: %s", c.Config.DNSZone)
	}

	if len(c.Config.NodeZones) == 0 {
		return fmt.Errorf("must specify at least one NodeZone")
	}

	if len(c.Config.MasterZones) == 0 {
		return fmt.Errorf("must specify at least one MasterZone")
	}

	// Check for master zone duplicates
	{
		masterZones := make(map[string]bool)
		for _, z := range c.Config.MasterZones {
			if masterZones[z] {
				return fmt.Errorf("MasterZones contained a duplicate value:  %v", z)
			}
			masterZones[z] = true
		}
	}

	// Check for node zone duplicates
	{
		nodeZones := make(map[string]bool)
		for _, z := range c.Config.NodeZones {
			if nodeZones[z] {
				return fmt.Errorf("NodeZones contained a duplicate value:  %v", z)
			}
			nodeZones[z] = true
		}
	}

	if (len(c.Config.MasterZones) % 2) == 0 {
		// Not technically a requirement, but doesn't really make sense to allow
		return fmt.Errorf("There should be an odd number of master-zones, for etcd's quorum.  Hint: Use -zone and -master-zone to declare node zones and master zones separately.")
	}

	if c.StateStore == nil {
		return fmt.Errorf("StateStore is required")
	}

	if c.Config.CloudProvider == "" {
		return fmt.Errorf("-cloud is required (e.g. aws, gce)")
	}

	tags := make(map[string]struct{})

	l := &cloudup.Loader{}
	l.Init()

	caStore := c.StateStore.CA()
	secrets := c.StateStore.Secrets()

	if c.Config.KubernetesVersion == "" {
		stableURL := "https://storage.googleapis.com/kubernetes-release/release/stable.txt"
		b, err := utils.ReadLocation(stableURL)
		if err != nil {
			return fmt.Errorf("-kubernetes-version not specified, and unable to download latest version from %q: %v", stableURL, err)
		}
		latestVersion := strings.TrimSpace(string(b))
		glog.Infof("Using kubernetes latest stable version: %s", latestVersion)

		c.Config.KubernetesVersion = latestVersion
		//return fmt.Errorf("Must either specify a KubernetesVersion (-kubernetes-version) or provide an asset with the release bundle")
	}

	// Normalize k8s version
	versionWithoutV := strings.TrimSpace(c.Config.KubernetesVersion)
	if strings.HasPrefix(versionWithoutV, "v") {
		versionWithoutV = versionWithoutV[1:]
	}
	if c.Config.KubernetesVersion != versionWithoutV {
		glog.Warningf("Normalizing kubernetes version: %q -> %q", c.Config.KubernetesVersion, versionWithoutV)
		c.Config.KubernetesVersion = versionWithoutV
	}

	if len(c.Config.Assets) == 0 {
		//defaultReleaseAsset := fmt.Sprintf("https://storage.googleapis.com/kubernetes-release/release/v%s/kubernetes-server-linux-amd64.tar.gz", c.Config.KubernetesVersion)
		//glog.Infof("Adding default kubernetes release asset: %s", defaultReleaseAsset)

		defaultKubeletAsset := fmt.Sprintf("https://storage.googleapis.com/kubernetes-release/release/v%s/bin/linux/amd64/kubelet", c.Config.KubernetesVersion)
		glog.Infof("Adding default kubelet release asset: %s", defaultKubeletAsset)

		defaultKubectlAsset := fmt.Sprintf("https://storage.googleapis.com/kubernetes-release/release/v%s/bin/linux/amd64/kubectl", c.Config.KubernetesVersion)
		glog.Infof("Adding default kubelet release asset: %s", defaultKubectlAsset)

		// TODO: Verify assets exist, get the hash (that will check that KubernetesVersion is valid)

		c.Config.Assets = append(c.Config.Assets, defaultKubeletAsset, defaultKubectlAsset)
	}

	if c.Config.NodeUp.Location == "" {
		location := "https://kubeupv2.s3.amazonaws.com/nodeup/nodeup.tar.gz"
		glog.Infof("Using default nodeup location: %q", location)
		c.Config.NodeUp.Location = location
	}

	var cloud fi.Cloud

	var project string
	var region string

	checkExisting := true

	c.Config.NodeUpTags = append(c.Config.NodeUpTags, "_jessie", "_debian_family", "_systemd")

	if useProtokube {
		tags["_protokube"] = struct{}{}
		c.Config.NodeUpTags = append(c.Config.NodeUpTags, "_protokube")
	} else {
		tags["_not_protokube"] = struct{}{}
		c.Config.NodeUpTags = append(c.Config.NodeUpTags, "_not_protokube")
	}

	if useMasterASG {
		tags["_master_asg"] = struct{}{}
	} else {
		tags["_master_single"] = struct{}{}
	}

	if useMasterLB {
		tags["_master_lb"] = struct{}{}
	} else {
		tags["_not_master_lb"] = struct{}{}
	}

	if c.Config.MasterPublicName != "" {
		tags["_master_dns"] = struct{}{}
	}

	l.AddTypes(map[string]interface{}{
		"keypair": &fitasks.Keypair{},
	})

	switch c.Config.CloudProvider {
	case "gce":
		{
			tags["_gce"] = struct{}{}
			c.Config.NodeUpTags = append(c.Config.NodeUpTags, "_gce")

			l.AddTypes(map[string]interface{}{
				"persistentDisk":       &gcetasks.PersistentDisk{},
				"instance":             &gcetasks.Instance{},
				"instanceTemplate":     &gcetasks.InstanceTemplate{},
				"network":              &gcetasks.Network{},
				"managedInstanceGroup": &gcetasks.ManagedInstanceGroup{},
				"firewallRule":         &gcetasks.FirewallRule{},
				"ipAddress":            &gcetasks.IPAddress{},
			})

			// For now a zone to be specified...
			// This will be replace with a region when we go full HA
			zone := c.Config.NodeZones[0]
			if zone == "" {
				return fmt.Errorf("Must specify a zone (use -zone)")
			}
			tokens := strings.Split(zone, "-")
			if len(tokens) <= 2 {
				return fmt.Errorf("Invalid Zone: %v", zone)
			}
			region = tokens[0] + "-" + tokens[1]

			project = c.Config.Project
			if project == "" {
				return fmt.Errorf("project is required for GCE")
			}
			gceCloud, err := gce.NewGCECloud(region, project)
			if err != nil {
				return err
			}
			cloud = gceCloud
		}

	case "aws":
		{
			tags["_aws"] = struct{}{}
			c.Config.NodeUpTags = append(c.Config.NodeUpTags, "_aws")

			l.AddTypes(map[string]interface{}{
				// EC2
				"elasticIP":                   &awstasks.ElasticIP{},
				"instance":                    &awstasks.Instance{},
				"instanceElasticIPAttachment": &awstasks.InstanceElasticIPAttachment{},
				"instanceVolumeAttachment":    &awstasks.InstanceVolumeAttachment{},
				"ebsVolume":                   &awstasks.EBSVolume{},
				"sshKey":                      &awstasks.SSHKey{},

				// IAM
				"iamInstanceProfile":     &awstasks.IAMInstanceProfile{},
				"iamInstanceProfileRole": &awstasks.IAMInstanceProfileRole{},
				"iamRole":                &awstasks.IAMRole{},
				"iamRolePolicy":          &awstasks.IAMRolePolicy{},

				// VPC / Networking
				"dhcpOptions":               &awstasks.DHCPOptions{},
				"internetGateway":           &awstasks.InternetGateway{},
				"internetGatewayAttachment": &awstasks.InternetGatewayAttachment{},
				"route":                     &awstasks.Route{},
				"routeTable":                &awstasks.RouteTable{},
				"routeTableAssociation":     &awstasks.RouteTableAssociation{},
				"securityGroup":             &awstasks.SecurityGroup{},
				"securityGroupRule":         &awstasks.SecurityGroupRule{},
				"subnet":                    &awstasks.Subnet{},
				"vpc":                       &awstasks.VPC{},
				"vpcDHDCPOptionsAssociation": &awstasks.VPCDHCPOptionsAssociation{},

				// ELB
				"loadBalancer":             &awstasks.LoadBalancer{},
				"loadBalancerAttachment":   &awstasks.LoadBalancerAttachment{},
				"loadBalancerHealthChecks": &awstasks.LoadBalancerHealthChecks{},

				// Autoscaling
				"autoscalingGroup":    &awstasks.AutoscalingGroup{},
				"launchConfiguration": &awstasks.LaunchConfiguration{},

				// Route53
				"dnsName": &awstasks.DNSName{},
				"dnsZone": &awstasks.DNSZone{},
			})

			if len(c.Config.NodeZones) == 0 {
				// TODO: Auto choose zones from region?
				return fmt.Errorf("Must specify a zone (use -zone)")
			}
			if len(c.Config.MasterZones) == 0 {
				return fmt.Errorf("Must specify a master zones")
			}

			nodeZones := make(map[string]bool)
			for _, zone := range c.Config.NodeZones {
				if len(zone) <= 2 {
					return fmt.Errorf("Invalid AWS zone: %q", zone)
				}

				nodeZones[zone] = true

				region = zone[:len(zone)-1]
				if c.Config.Region != "" && c.Config.Region != region {
					return fmt.Errorf("Clusters cannot span multiple regions")
				}

				c.Config.Region = region
			}

			for _, zone := range c.Config.MasterZones {
				if !nodeZones[zone] {
					// We could relax this, but this seems like a reasonable constraint
					return fmt.Errorf("All MasterZones must (currently) also be NodeZones")
				}
			}

			err := awsup.ValidateRegion(region)
			if err != nil {
				return err
			}

			if c.SSHPublicKey == "" {
				return fmt.Errorf("SSH public key must be specified when running with AWS")
			}

			cloudTags := map[string]string{"KubernetesCluster": c.Config.ClusterName}

			awsCloud, err := awsup.NewAWSCloud(region, cloudTags)
			if err != nil {
				return err
			}

			err = awsCloud.ValidateZones(c.Config.NodeZones)
			if err != nil {
				return err
			}
			cloud = awsCloud

			l.TemplateFunctions["MachineTypeInfo"] = awsup.GetMachineTypeInfo
		}

	default:
		return fmt.Errorf("unknown CloudProvider %q", c.Config.CloudProvider)
	}

	l.Tags = tags
	l.WorkDir = c.WorkDir
	l.NodeModelDir = c.NodeModelDir
	l.OptionsLoader = loader.NewOptionsLoader(c.Config)

	l.TemplateFunctions["HasTag"] = func(tag string) bool {
		_, found := l.Tags[tag]
		return found
	}

	// TODO: Sort this out...
	l.OptionsLoader.TemplateFunctions["HasTag"] = l.TemplateFunctions["HasTag"]

	l.TemplateFunctions["CA"] = func() fi.CAStore {
		return caStore
	}
	l.TemplateFunctions["Secrets"] = func() fi.SecretStore {
		return secrets
	}
	l.TemplateFunctions["GetOrCreateSecret"] = func(id string) (string, error) {
		secret, _, err := secrets.GetOrCreateSecret(id)
		if err != nil {
			return "", fmt.Errorf("error creating secret %q: %v", id, err)
		}

		return secret.AsString()
	}

	if c.SSHPublicKey != "" {
		authorized, err := ioutil.ReadFile(c.SSHPublicKey)
		if err != nil {
			return fmt.Errorf("error reading SSH key file %q: %v", c.SSHPublicKey, err)
		}

		l.Resources["ssh-public-key"] = fi.NewStringResource(string(authorized))
	}

	taskMap, err := l.Build(c.ModelDirs)
	if err != nil {
		glog.Exitf("error building: %v", err)
	}

	var target fi.Target

	switch c.Target {
	case "direct":
		switch c.Config.CloudProvider {
		case "gce":
			target = gce.NewGCEAPITarget(cloud.(*gce.GCECloud))
		case "aws":
			target = awsup.NewAWSAPITarget(cloud.(*awsup.AWSCloud))
		default:
			return fmt.Errorf("direct configuration not supported with CloudProvider:%q", c.Config.CloudProvider)
		}

	case "terraform":
		checkExisting = false
		outDir := path.Join(c.WorkDir, "terraform")
		target = terraform.NewTerraformTarget(cloud, region, project, outDir)

	case "dryrun":
		target = fi.NewDryRunTarget(os.Stdout)
	default:
		return fmt.Errorf("unsupported target type %q", c.Target)
	}

	context, err := fi.NewContext(target, cloud, caStore, checkExisting)
	if err != nil {
		glog.Exitf("error building context: %v", err)
	}
	defer context.Close()

	err = context.RunTasks(taskMap)
	if err != nil {
		glog.Exitf("error running tasks: %v", err)
	}

	err = target.Finish(taskMap)
	if err != nil {
		glog.Exitf("error closing target: %v", err)
	}

	return nil
}
