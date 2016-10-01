package kutil

import (
	"encoding/base64"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/golang/glog"
	"k8s.io/kops/upup/pkg/api"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/cloudup"
	"k8s.io/kops/upup/pkg/fi/cloudup/awsup"
	"strconv"
	"strings"
)

// ImportCluster tries to reverse engineer an existing k8s cluster, adding it to the cluster registry
type ImportCluster struct {
	ClusterName string
	Cloud       fi.Cloud

	ClusterRegistry *api.ClusterRegistry
}

func (x *ImportCluster) ImportAWSCluster() error {
	awsCloud := x.Cloud.(awsup.AWSCloud)
	clusterName := x.ClusterName

	if clusterName == "" {
		return fmt.Errorf("ClusterName must be specified")
	}

	var instanceGroups []*api.InstanceGroup

	cluster := &api.Cluster{}
	cluster.Annotations = make(map[string]string)

	cluster.Annotations[api.AnnotationNameManagement] = api.AnnotationValueManagementImported

	cluster.Spec.CloudProvider = string(fi.CloudProviderAWS)
	cluster.Name = clusterName

	cluster.Spec.KubeControllerManager = &api.KubeControllerManagerConfig{}

	cluster.Spec.Channel = api.DefaultChannel

	channel, err := api.LoadChannel(cluster.Spec.Channel)
	if err != nil {
		return err
	}

	masterGroup := &api.InstanceGroup{}
	masterGroup.Spec.Role = api.InstanceGroupRoleMaster
	masterGroup.Spec.MinSize = fi.Int(1)
	masterGroup.Spec.MaxSize = fi.Int(1)
	instanceGroups = append(instanceGroups, masterGroup)

	instances, err := findInstances(awsCloud)
	if err != nil {
		return fmt.Errorf("error finding instances: %v", err)
	}

	var masterInstance *ec2.Instance
	zones := make(map[string]*api.ClusterZoneSpec)

	for _, instance := range instances {
		instanceState := aws.StringValue(instance.State.Name)

		if instanceState != "terminated" && instance.Placement != nil {
			zoneName := aws.StringValue(instance.Placement.AvailabilityZone)
			zone := zones[zoneName]
			if zone == nil {
				zone = &api.ClusterZoneSpec{Name: zoneName}
				zones[zoneName] = zone
			}

			subnet := aws.StringValue(instance.SubnetId)
			if subnet != "" {
				zone.ProviderID = subnet
			}
		}

		role, _ := awsup.FindEC2Tag(instance.Tags, "Role")
		if role == clusterName+"-master" {
			if masterInstance != nil {
				masterState := aws.StringValue(masterInstance.State.Name)

				glog.Infof("Found multiple masters: %s and %s", masterState, instanceState)

				if masterState == "terminated" && instanceState != "terminated" {
					// OK
				} else if instanceState == "terminated" && masterState != "terminated" {
					// Ignore this one
					continue
				} else {
					return fmt.Errorf("found multiple masters")
				}
			}
			masterInstance = instance
		}
	}
	if masterInstance == nil {
		return fmt.Errorf("could not find master node")
	}
	masterInstanceID := aws.StringValue(masterInstance.InstanceId)
	glog.Infof("Found master: %q", masterInstanceID)

	masterGroup.Spec.MachineType = aws.StringValue(masterInstance.InstanceType)

	subnets, err := DescribeSubnets(x.Cloud)
	if err != nil {
		return fmt.Errorf("error finding subnets: %v", err)
	}

	for _, s := range subnets {
		subnetID := aws.StringValue(s.SubnetId)

		found := false
		for _, zone := range zones {
			if zone.ProviderID == subnetID {
				zone.CIDR = aws.StringValue(s.CidrBlock)
				found = true
			}
		}

		if !found {
			glog.Warningf("Ignoring subnet %q in which no instances were found", subnetID)
		}
	}
	for k, zone := range zones {
		if zone.ProviderID == "" {
			return fmt.Errorf("cannot find subnet %q.  Please report this issue", k)
		}
		if zone.CIDR == "" {
			return fmt.Errorf("cannot find subnet %q.  If you used an existing subnet, please tag it with %s=%s and retry the import", zone.ProviderID, awsup.TagClusterName, clusterName)
		}
	}

	vpcID := aws.StringValue(masterInstance.VpcId)
	var vpc *ec2.Vpc
	{
		vpc, err = awsCloud.DescribeVPC(vpcID)
		if err != nil {
			return err
		}
		if vpc == nil {
			return fmt.Errorf("cannot find vpc %q", vpcID)
		}
	}

	cluster.Spec.NetworkID = vpcID
	cluster.Spec.NetworkCIDR = aws.StringValue(vpc.CidrBlock)
	for _, zone := range zones {
		cluster.Spec.Zones = append(cluster.Spec.Zones, zone)
	}

	masterZone := zones[aws.StringValue(masterInstance.Placement.AvailabilityZone)]
	if masterZone == nil {
		return fmt.Errorf("cannot find zone %q for master.  Please report this issue", aws.StringValue(masterInstance.Placement.AvailabilityZone))
	}
	masterGroup.Spec.Zones = []string{masterZone.Name}
	masterGroup.Name = "master-" + masterZone.Name

	userData, err := GetInstanceUserData(awsCloud, aws.StringValue(masterInstance.InstanceId))
	if err != nil {
		return fmt.Errorf("error getting master user-data: %v", err)
	}

	conf, err := ParseUserDataConfiguration(userData)
	if err != nil {
		return fmt.Errorf("error parsing master user-data: %v", err)
	}

	//master := &NodeSSH{
	//	Hostname: c.Master,
	//}
	//err := master.AddSSHIdentity(c.SSHIdentity)
	//if err != nil {
	//	return err
	//}
	//
	//
	//fmt.Printf("Connecting to node on %s\n", c.Node)
	//
	//node := &NodeSSH{
	//	Hostname: c.Node,
	//}
	//err = node.AddSSHIdentity(c.SSHIdentity)
	//if err != nil {
	//	return err
	//}

	instancePrefix := conf.Settings["INSTANCE_PREFIX"]
	if instancePrefix == "" {
		return fmt.Errorf("cannot determine INSTANCE_PREFIX")
	}
	if instancePrefix != clusterName {
		return fmt.Errorf("INSTANCE_PREFIX %q did not match cluster name %q", instancePrefix, clusterName)
	}

	//k8s.NodeMachineType, err = InstanceType(node)
	//if err != nil {
	//	return fmt.Errorf("cannot determine node instance type: %v", err)
	//}

	// We want to upgrade!
	// k8s.ImageId = ""

	//clusterConfig.ClusterIPRange = conf.Settings["CLUSTER_IP_RANGE"]
	cluster.Spec.KubeControllerManager.AllocateNodeCIDRs = conf.ParseBool("ALLOCATE_NODE_CIDRS")
	//clusterConfig.KubeUser = conf.Settings["KUBE_USER"]
	cluster.Spec.ServiceClusterIPRange = conf.Settings["SERVICE_CLUSTER_IP_RANGE"]
	cluster.Spec.NonMasqueradeCIDR = conf.Settings["NON_MASQUERADE_CIDR"]
	//clusterConfig.EnableClusterMonitoring = conf.Settings["ENABLE_CLUSTER_MONITORING"]
	//clusterConfig.EnableClusterLogging = conf.ParseBool("ENABLE_CLUSTER_LOGGING")
	//clusterConfig.EnableNodeLogging = conf.ParseBool("ENABLE_NODE_LOGGING")
	//clusterConfig.LoggingDestination = conf.Settings["LOGGING_DESTINATION"]
	//clusterConfig.ElasticsearchLoggingReplicas, err = parseInt(conf.Settings["ELASTICSEARCH_LOGGING_REPLICAS"])
	//if err != nil {
	//	return fmt.Errorf("cannot parse ELASTICSEARCH_LOGGING_REPLICAS=%q: %v", conf.Settings["ELASTICSEARCH_LOGGING_REPLICAS"], err)
	//}
	//clusterConfig.EnableClusterDNS = conf.ParseBool("ENABLE_CLUSTER_DNS")
	//clusterConfig.EnableClusterUI = conf.ParseBool("ENABLE_CLUSTER_UI")
	//clusterConfig.DNSReplicas, err = parseInt(conf.Settings["DNS_REPLICAS"])
	//if err != nil {
	//	return fmt.Errorf("cannot parse DNS_REPLICAS=%q: %v", conf.Settings["DNS_REPLICAS"], err)
	//}
	//clusterConfig.DNSServerIP = conf.Settings["DNS_SERVER_IP"]
	cluster.Spec.ClusterDNSDomain = conf.Settings["DNS_DOMAIN"]
	//clusterConfig.AdmissionControl = conf.Settings["ADMISSION_CONTROL"]
	//clusterConfig.MasterIPRange = conf.Settings["MASTER_IP_RANGE"]
	//clusterConfig.DNSServerIP = conf.Settings["DNS_SERVER_IP"]
	//clusterConfig.DockerStorage = conf.Settings["DOCKER_STORAGE"]
	//k8s.MasterExtraSans = conf.Settings["MASTER_EXTRA_SANS"] // Not user set

	nodeGroup := &api.InstanceGroup{}
	nodeGroup.Spec.Role = api.InstanceGroupRoleNode
	nodeGroup.Name = "nodes"
	for _, zone := range zones {
		nodeGroup.Spec.Zones = append(nodeGroup.Spec.Zones, zone.Name)
	}
	instanceGroups = append(instanceGroups, nodeGroup)

	//primaryNodeSet.Spec.MinSize, err = conf.ParseInt("NUM_MINIONS")
	//if err != nil {
	//	return fmt.Errorf("cannot parse NUM_MINIONS=%q: %v", conf.Settings["NUM_MINIONS"], err)
	//}

	{
		groups, err := findAutoscalingGroups(awsCloud, awsCloud.Tags())
		if err != nil {
			return fmt.Errorf("error listing autoscaling groups: %v", err)
		}

		if len(groups) == 0 {
			glog.Warningf("No Autoscaling group found")
		}
		if len(groups) == 1 {
			glog.Warningf("Multiple Autoscaling groups found")
		}
		minSize := 0
		maxSize := 0
		for _, group := range groups {
			minSize += int(aws.Int64Value(group.MinSize))
			maxSize += int(aws.Int64Value(group.MaxSize))
		}
		if minSize != 0 {
			nodeGroup.Spec.MinSize = fi.Int(minSize)
		}
		if maxSize != 0 {
			nodeGroup.Spec.MaxSize = fi.Int(maxSize)
		}

		// Determine the machine type
		for _, group := range groups {
			name := aws.StringValue(group.LaunchConfigurationName)
			launchConfiguration, err := findAutoscalingLaunchConfiguration(awsCloud, name)
			if err != nil {
				return fmt.Errorf("error finding autoscaling LaunchConfiguration %q: %v", name, err)
			}

			if launchConfiguration == nil {
				glog.Warningf("LaunchConfiguration %q not found; ignoring", name)
				continue
			}

			nodeGroup.Spec.MachineType = aws.StringValue(launchConfiguration.InstanceType)
			break
		}
	}

	if conf.Version == "1.1" {
		// If users went with defaults on some things, clear them out so they get the new defaults
		//if clusterConfig.AdmissionControl == "NamespaceLifecycle,LimitRanger,SecurityContextDeny,ServiceAccount,ResourceQuota" {
		//	// More admission controllers in 1.2
		//	clusterConfig.AdmissionControl = ""
		//}
		if masterGroup.Spec.MachineType == "t2.micro" {
			// Different defaults in 1.2
			masterGroup.Spec.MachineType = ""
		}
		if nodeGroup.Spec.MachineType == "t2.micro" {
			// Encourage users to pick something better...
			nodeGroup.Spec.MachineType = ""
		}
	}
	if conf.Version == "1.2" {
		// If users went with defaults on some things, clear them out so they get the new defaults
		//if clusterConfig.AdmissionControl == "NamespaceLifecycle,LimitRanger,ServiceAccount,PersistentVolumeLabel,ResourceQuota" {
		//	// More admission controllers in 1.2
		//	clusterConfig.AdmissionControl = ""
		//}
	}

	for _, etcdClusterName := range []string{"main", "events"} {
		etcdCluster := &api.EtcdClusterSpec{
			Name: etcdClusterName,
		}
		for _, az := range masterGroup.Spec.Zones {
			etcdCluster.Members = append(etcdCluster.Members, &api.EtcdMemberSpec{
				Name: az,
				Zone: fi.String(az),
			})
		}
		cluster.Spec.EtcdClusters = append(cluster.Spec.EtcdClusters, etcdCluster)
	}

	//if masterInstance.PublicIpAddress != nil {
	//	eip, err := findElasticIP(cloud, *masterInstance.PublicIpAddress)
	//	if err != nil {
	//		return err
	//	}
	//	if eip != nil {
	//		k8s.MasterElasticIP = masterInstance.PublicIpAddress
	//	}
	//}
	//
	//vpc, err := cloud.DescribeVPC(*k8s.VPCID)
	//if err != nil {
	//	return err
	//}
	//k8s.DHCPOptionsID = vpc.DhcpOptionsId
	//
	//igw, err := findInternetGateway(cloud, *k8s.VPCID)
	//if err != nil {
	//	return err
	//}
	//if igw == nil {
	//	return fmt.Errorf("unable to find internet gateway for VPC %q", k8s.VPCID)
	//}
	//k8s.InternetGatewayID = igw.InternetGatewayId
	//
	//rt, err := findRouteTable(cloud, *k8s.SubnetID)
	//if err != nil {
	//	return err
	//}
	//if rt == nil {
	//	return fmt.Errorf("unable to find route table for Subnet %q", k8s.SubnetID)
	//}
	//k8s.RouteTableID = rt.RouteTableId

	//b.Context = "aws_" + instancePrefix

	keyStore := x.ClusterRegistry.KeyStore(clusterName)

	//caCert, err := masterSSH.Join("ca.crt").ReadFile()
	caCert, err := conf.ParseCert("CA_CERT")
	if err != nil {
		return err
	}
	err = keyStore.AddCert(fi.CertificateId_CA, caCert)
	if err != nil {
		return err
	}

	////masterKey, err := masterSSH.Join("server.key").ReadFile()
	//masterKey, err := conf.ParseKey("MASTER_KEY")
	//if err != nil {
	//	return err
	//}
	////masterCert, err := masterSSH.Join("server.cert").ReadFile()
	//masterCert, err := conf.ParseCert("MASTER_CERT")
	//if err != nil {
	//	return err
	//}
	//err = keyStore.ImportKeypair("master", masterKey, masterCert)
	//if err != nil {
	//	return err
	//}
	//
	////kubeletKey, err := kubeletSSH.Join("kubelet.key").ReadFile()
	//kubeletKey, err := conf.ParseKey("KUBELET_KEY")
	//if err != nil {
	//	return err
	//}
	////kubeletCert, err := kubeletSSH.Join("kubelet.cert").ReadFile()
	//kubeletCert, err := conf.ParseCert("KUBELET_CERT")
	//if err != nil {
	//	return err
	//}
	//err = keyStore.ImportKeypair("kubelet", kubeletKey, kubeletCert)
	//if err != nil {
	//	return err
	//}

	// We don't store the kubecfg key
	//kubecfgKey, err := masterSSH.Join("kubecfg.key").ReadFile()
	//if err != nil {
	//	return err
	//}
	//kubecfgCert, err := masterSSH.Join("kubecfg.cert").ReadFile()
	//if err != nil {
	//	return err
	//}
	//err = keyStore.ImportKeypair("kubecfg", kubecfgKey, kubecfgCert)
	//if err != nil {
	//	return err
	//}

	//// We will generate new tokens, but some of these are in existing API objects
	//secretStore := x.StateStore.Secrets()
	//kubePassword := conf.Settings["KUBE_PASSWORD"]
	//kubeletToken = conf.Settings["KUBELET_TOKEN"]
	//kubeProxyToken = conf.Settings["KUBE_PROXY_TOKEN"]

	var fullInstanceGroups []*api.InstanceGroup
	for _, ig := range instanceGroups {
		full, err := cloudup.PopulateInstanceGroupSpec(cluster, ig, channel)
		if err != nil {
			return err
		}
		fullInstanceGroups = append(fullInstanceGroups, full)
	}

	err = api.CreateClusterConfig(x.ClusterRegistry, cluster, fullInstanceGroups)
	if err != nil {
		return err
	}

	// Note - we can't PopulateClusterSpec & WriteCompletedConfig, because the cluster doesn't have a valid DNS Name
	//fullCluster, err := cloudup.PopulateClusterSpec(cluster, x.ClusterRegistry)
	//if err != nil {
	//	return err
	//}
	//
	//err = x.ClusterRegistry.WriteCompletedConfig(fullCluster)
	//if err != nil {
	//	return fmt.Errorf("error writing completed cluster spec: %v", err)
	//}

	return nil
}

func parseInt(s string) (int, error) {
	if s == "" {
		return 0, nil
	}

	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, err
	}

	return int(n), nil
}

//func writeConf(p string, k8s *cloudup.CloudConfig) error {
//	jsonBytes, err := json.Marshal(k8s)
//	if err != nil {
//		return fmt.Errorf("error serializing configuration (json write phase): %v", err)
//	}
//
//	var confObj interface{}
//	err = yaml.Unmarshal(jsonBytes, &confObj)
//	if err != nil {
//		return fmt.Errorf("error serializing configuration (yaml read phase): %v", err)
//	}
//
//	m := confObj.(map[interface{}]interface{})
//
//	for k, v := range m {
//		if v == nil {
//			delete(m, k)
//		}
//		s, ok := v.(string)
//		if ok && s == "" {
//			delete(m, k)
//		}
//		//glog.Infof("%v=%v", k, v)
//	}
//
//	yaml, err := yaml.Marshal(confObj)
//	if err != nil {
//		return fmt.Errorf("error serializing configuration (yaml write phase): %v", err)
//	}
//
//	err = ioutil.WriteFile(p, yaml, 0600)
//	if err != nil {
//		return fmt.Errorf("error writing configuration to file %q: %v", p, err)
//	}
//
//	return nil
//}
//
//func findInternetGateway(cloud awsup.AWSCloud, vpcID string) (*ec2.InternetGateway, error) {
//	request := &ec2.DescribeInternetGatewaysInput{
//		Filters: []*ec2.Filter{fi.NewEC2Filter("attachment.vpc-id", vpcID)},
//	}
//
//	response, err := cloud.EC2.DescribeInternetGateways(request)
//	if err != nil {
//		return nil, fmt.Errorf("error listing InternetGateways: %v", err)
//	}
//	if response == nil || len(response.InternetGateways) == 0 {
//		return nil, nil
//	}
//
//	if len(response.InternetGateways) != 1 {
//		return nil, fmt.Errorf("found multiple InternetGatewayAttachments to VPC")
//	}
//	igw := response.InternetGateways[0]
//	return igw, nil
//}

//func findRouteTable(cloud awsup.AWSCloud, subnetID string) (*ec2.RouteTable, error) {
//	request := &ec2.DescribeRouteTablesInput{
//		Filters: []*ec2.Filter{fi.NewEC2Filter("association.subnet-id", subnetID)},
//	}
//
//	response, err := cloud.EC2.DescribeRouteTables(request)
//	if err != nil {
//		return nil, fmt.Errorf("error listing RouteTables: %v", err)
//	}
//	if response == nil || len(response.RouteTables) == 0 {
//		return nil, nil
//	}
//
//	if len(response.RouteTables) != 1 {
//		return nil, fmt.Errorf("found multiple RouteTables matching tags")
//	}
//	rt := response.RouteTables[0]
//	return rt, nil
//}
//
//func findElasticIP(cloud awsup.AWSCloud, publicIP string) (*ec2.Address, error) {
//	request := &ec2.DescribeAddressesInput{
//		PublicIps: []*string{&publicIP},
//	}
//
//	response, err := cloud.EC2.DescribeAddresses(request)
//	if err != nil {
//		if awsErr, ok := err.(awserr.Error); ok {
//			if awsErr.Code() == "InvalidAddress.NotFound" {
//				return nil, nil
//			}
//		}
//		return nil, fmt.Errorf("error listing Addresses: %v", err)
//	}
//	if response == nil || len(response.Addresses) == 0 {
//		return nil, nil
//	}
//
//	if len(response.Addresses) != 1 {
//		return nil, fmt.Errorf("found multiple Addresses matching IP %q", publicIP)
//	}
//	return response.Addresses[0], nil
//}

func findInstances(c awsup.AWSCloud) ([]*ec2.Instance, error) {
	filters := buildEC2Filters(c)

	request := &ec2.DescribeInstancesInput{
		Filters: filters,
	}

	glog.V(2).Infof("Querying EC2 instances")

	var instances []*ec2.Instance

	err := c.EC2().DescribeInstancesPages(request, func(p *ec2.DescribeInstancesOutput, lastPage bool) bool {
		for _, reservation := range p.Reservations {
			for _, instance := range reservation.Instances {
				instances = append(instances, instance)
			}
		}
		return true
	})
	if err != nil {
		return nil, fmt.Errorf("error describing instances: %v", err)
	}

	return instances, nil
}

//func GetMetadata(t *NodeSSH, key string) (string, error) {
//	b, err := t.exec("curl -s http://169.254.169.254/latest/meta-data/" + key)
//	if err != nil {
//		return "", fmt.Errorf("error querying for metadata %q: %v", key, err)
//	}
//	return string(b), nil
//}
//
//func InstanceType(t *NodeSSH) (string, error) {
//	return GetMetadata(t, "instance-type")
//}
//
//func GetMetadataList(t *NodeSSH, key string) ([]string, error) {
//	d, err := GetMetadata(t, key)
//	if err != nil {
//		return nil, err
//	}
//	var macs []string
//	for _, line := range strings.Split(d, "\n") {
//		mac := line
//		mac = strings.Trim(mac, "/")
//		mac = strings.TrimSpace(mac)
//		if mac == "" {
//			continue
//		}
//		macs = append(macs, mac)
//	}
//
//	return macs, nil
//}

// Fetch instance UserData
func GetInstanceUserData(cloud awsup.AWSCloud, instanceID string) ([]byte, error) {
	request := &ec2.DescribeInstanceAttributeInput{}
	request.InstanceId = aws.String(instanceID)
	request.Attribute = aws.String("userData")
	response, err := cloud.EC2().DescribeInstanceAttribute(request)
	if err != nil {
		return nil, fmt.Errorf("error querying EC2 for user metadata for instance %q: %v", instanceID, err)
	}
	if response.UserData != nil {
		b, err := base64.StdEncoding.DecodeString(aws.StringValue(response.UserData.Value))
		if err != nil {
			return nil, fmt.Errorf("error decoding EC2 UserData: %v", err)
		}
		return b, nil
	}
	return nil, nil
}

type UserDataConfiguration struct {
	Version  string
	Settings map[string]string
}

func (u *UserDataConfiguration) ParseBool(key string) *bool {
	s := u.Settings[key]
	if s == "" {
		return nil
	}
	s = strings.ToLower(s)
	if s == "true" || s == "1" || s == "y" || s == "yes" || s == "t" {
		return fi.Bool(true)
	}
	return fi.Bool(false)
}

func (u *UserDataConfiguration) ParseInt(key string) (*int, error) {
	s := u.Settings[key]
	if s == "" {
		return nil, nil
	}

	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("error parsing key %q=%q", key, s)
	}

	return fi.Int(int(n)), nil
}

func (u *UserDataConfiguration) ParseCert(key string) (*fi.Certificate, error) {
	s := u.Settings[key]
	if s == "" {
		return nil, nil
	}

	data, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("error decoding base64 certificate %q: %v", key, err)
	}
	cert, err := fi.LoadPEMCertificate(data)
	if err != nil {
		return nil, fmt.Errorf("error parsing certificate %q: %v", key, err)
	}

	return cert, nil
}

func (u *UserDataConfiguration) ParseKey(key string) (*fi.PrivateKey, error) {
	s := u.Settings[key]
	if s == "" {
		return nil, nil
	}

	data, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("error decoding base64 private key %q: %v", key, err)
	}
	k, err := fi.ParsePEMPrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("error parsing private key %q: %v", key, err)
	}

	return k, nil
}

func ParseUserDataConfiguration(raw []byte) (*UserDataConfiguration, error) {
	userData, err := UserDataToString(raw)
	if err != nil {
		return nil, err
	}
	settings := make(map[string]string)

	version := ""
	if strings.Contains(userData, "install-salt master") || strings.Contains(userData, "dpkg -s salt-minion") {
		version = "1.1"
	} else {
		version = "1.2"
	}
	if version == "1.1" {
		for _, line := range strings.Split(userData, "\n") {
			if !strings.HasPrefix(line, "readonly ") {
				continue
			}
			line = line[9:]
			sep := strings.Index(line, "=")
			k := ""
			v := ""
			if sep != -1 {
				k = line[0:sep]
				v = line[sep+1:]
			}

			if k == "" {
				glog.V(4).Infof("Unknown line: %s", line)
			}

			if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
				v = v[1 : len(v)-1]
			}
			settings[k] = v
		}
	} else {
		for _, line := range strings.Split(userData, "\n") {
			sep := strings.Index(line, ": ")
			k := ""
			v := ""
			if sep != -1 {
				k = line[0:sep]
				v = line[sep+2:]
			}

			if k == "" {
				glog.V(4).Infof("Unknown line: %s", line)
			}

			if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
				v = v[1 : len(v)-1]
			} else if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
				v = v[1 : len(v)-1]
			}
			settings[k] = v
		}
	}

	c := &UserDataConfiguration{
		Version:  version,
		Settings: settings,
	}
	return c, nil
}

func UserDataToString(userData []byte) (string, error) {
	var err error
	if len(userData) > 2 && userData[0] == 31 && userData[1] == 139 {
		// GZIP
		glog.V(2).Infof("gzip data detected; will decompress")

		userData, err = gunzipBytes(userData)
		if err != nil {
			return "", fmt.Errorf("error decompressing user data: %v", err)
		}
	}
	return string(userData), nil
}
