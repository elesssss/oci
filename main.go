/*
甲骨文云 - 创建实例工具
用法:
  -arm   创建 VM.Standard.A1.Flex (ARM架构)
  -amd   创建 VM.Standard.E2.1.Micro (AMD架构)
  -c     配置文件路径 (默认: ./config.ini)

示例:
  ./oci -arm
  ./oci -amd
  ./oci -arm -c /path/to/config.ini
*/
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/oracle/oci-go-sdk/v54/common"
	"github.com/oracle/oci-go-sdk/v54/core"
	"github.com/oracle/oci-go-sdk/v54/identity"
	"gopkg.in/ini.v1"
)

// ── 常量 ───────────────────────────────────────────────────────────────────────

const defConfigFilePath = "./config.ini"

// ── 全局变量 ────────────────────────────────────────────────────────────────────

var (
	configFilePath string
	provider       common.ConfigurationProvider
	computeClient  core.ComputeClient
	networkClient  core.VirtualNetworkClient
	identityClient identity.IdentityClient
	ctx            = context.Background()

	oracle   Oracle
	instance Instance

	proxy   string
	token   string
	chat_id string

	sendMessageUrl string
	editMessageUrl string
)

// ── 数据结构 ────────────────────────────────────────────────────────────────────

type Oracle struct {
	User         string `ini:"user"`
	Fingerprint  string `ini:"fingerprint"`
	Tenancy      string `ini:"tenancy"`
	Region       string `ini:"region"`
	Key_file     string `ini:"key_file"`
	Key_password string `ini:"key_password"`
}

type Instance struct {
	AvailabilityDomain     string  `ini:"availabilityDomain"`
	SSH_Public_Key         string  `ini:"ssh_authorized_key"`
	VcnDisplayName         string  `ini:"vcnDisplayName"`
	SubnetDisplayName      string  `ini:"subnetDisplayName"`
	Shape                  string  `ini:"shape"`
	OperatingSystem        string  `ini:"OperatingSystem"`
	OperatingSystemVersion string  `ini:"OperatingSystemVersion"`
	InstanceDisplayName    string  `ini:"instanceDisplayName"`
	Ocpus                  float32 `ini:"cpus"`
	MemoryInGBs            float32 `ini:"memoryInGBs"`
	Burstable              string  `ini:"burstable"`
	BootVolumeSizeInGBs    int64   `ini:"bootVolumeSizeInGBs"`
	Sum                    int32   `ini:"sum"`
	Retry                  int32   `ini:"retry"`
	CloudInit              string  `ini:"cloud-init"`
	MinTime                int32   `ini:"minTime"`
	MaxTime                int32   `ini:"maxTime"`
}

type Message struct {
	OK          bool   `json:"ok"`
	Result      Result `json:"result"`
	ErrorCode   int    `json:"error_code"`
	Description string `json:"description"`
}
type Result struct {
	MessageId int `json:"message_id"`
}

// ── 入口 ─────────────────────────────────────────────────────────────────────────

func main() {
	var useARM, useAMD bool
	flag.StringVar(&configFilePath, "config", defConfigFilePath, "配置文件路径")
	flag.StringVar(&configFilePath, "c", defConfigFilePath, "配置文件路径")
	flag.BoolVar(&useARM, "arm", false, "创建 VM.Standard.A1.Flex (ARM)")
	flag.BoolVar(&useAMD, "amd", false, "创建 VM.Standard.E2.1.Micro (AMD)")
	flag.Parse()

	if !useARM && !useAMD {
		fmt.Println("请指定 -arm 或 -amd 参数")
		fmt.Println("  -arm   创建 VM.Standard.A1.Flex")
		fmt.Println("  -amd   创建 VM.Standard.E2.1.Micro")
		return
	}

	// 读取配置文件
	cfg, err := ini.Load(configFilePath)
	if err != nil {
		fmt.Printf("\033[1;31m读取配置文件失败: %s\033[0m\n", err.Error())
		return
	}

	defSec := cfg.Section(ini.DefaultSection)
	proxy = defSec.Key("proxy").Value()
	token = defSec.Key("token").Value()
	chat_id = defSec.Key("chat_id").Value()
	sendMessageUrl = "https://api.telegram.org/bot" + token + "/sendMessage"
	editMessageUrl = "https://api.telegram.org/bot" + token + "/editMessageText"
	rand.Seed(time.Now().UnixNano())

	// 找到第一个有效的甲骨文账号配置
	var oracleSection *ini.Section
	for _, sec := range cfg.Sections() {
		if len(sec.ParentKeys()) == 0 {
			user := sec.Key("user").Value()
			fingerprint := sec.Key("fingerprint").Value()
			tenancy := sec.Key("tenancy").Value()
			region := sec.Key("region").Value()
			key_file := sec.Key("key_file").Value()
			if user != "" && fingerprint != "" && tenancy != "" && region != "" && key_file != "" {
				oracleSection = sec
				break
			}
		}
	}
	if oracleSection == nil {
		fmt.Println("\033[1;31m未找到有效的甲骨文账号配置，请检查配置文件\033[0m")
		return
	}

	// 初始化 Oracle 账号
	oracle = Oracle{}
	if err := oracleSection.MapTo(&oracle); err != nil {
		fmt.Printf("\033[1;31m解析账号配置失败: %s\033[0m\n", err.Error())
		return
	}
	provider, err = getProvider(oracle)
	if err != nil {
		fmt.Printf("\033[1;31m获取 Provider 失败: %s\033[0m\n", err.Error())
		return
	}
	computeClient, err = core.NewComputeClientWithConfigurationProvider(provider)
	if err != nil {
		fmt.Printf("\033[1;31m创建 ComputeClient 失败: %s\033[0m\n", err.Error())
		return
	}
	setProxyOrNot(&computeClient.BaseClient)
	networkClient, err = core.NewVirtualNetworkClientWithConfigurationProvider(provider)
	if err != nil {
		fmt.Printf("\033[1;31m创建 VirtualNetworkClient 失败: %s\033[0m\n", err.Error())
		return
	}
	setProxyOrNot(&networkClient.BaseClient)
	identityClient, err = identity.NewIdentityClientWithConfigurationProvider(provider)
	if err != nil {
		fmt.Printf("\033[1;31m创建 IdentityClient 失败: %s\033[0m\n", err.Error())
		return
	}
	setProxyOrNot(&identityClient.BaseClient)

	// 确定使用 ARM 还是 AMD 的实例配置段
	instanceBaseSection := cfg.Section("INSTANCE")
	var instanceSectionName string
	if useARM {
		instanceSectionName = "INSTANCE.ARM"
	} else {
		instanceSectionName = "INSTANCE.AMD"
	}

	// 合并 [INSTANCE] 和 [INSTANCE.ARM/AMD] 配置
	instance = Instance{}
	if err := instanceBaseSection.MapTo(&instance); err != nil {
		fmt.Printf("\033[1;31m解析 [INSTANCE] 配置失败: %s\033[0m\n", err.Error())
		return
	}
	instanceSection, err := cfg.GetSection(instanceSectionName)
	if err != nil {
		fmt.Printf("\033[1;31m未找到配置段 [%s]: %s\033[0m\n", instanceSectionName, err.Error())
		return
	}
	if err := instanceSection.MapTo(&instance); err != nil {
		fmt.Printf("\033[1;31m解析 [%s] 配置失败: %s\033[0m\n", instanceSectionName, err.Error())
		return
	}

	// 获取可用性域
	fmt.Println("正在获取可用性域...")
	ads, err := listAvailabilityDomains()
	if err != nil {
		fmt.Printf("\033[1;31m获取可用性域失败: %s\033[0m\n", err.Error())
		return
	}

	// 开始创建实例
	fmt.Printf("\033[1;36m[%s] 账号: %s  开始创建实例...\033[0m\n", instanceSectionName, oracleSection.Name())
	sum, num := launchInstances(ads, oracleSection.Name())
	fmt.Printf("\033[1;36m创建完成。总数: %d  成功: %d  失败: %d\033[0m\n", sum, num, sum-num)
}

// ── 创建实例核心逻辑 ──────────────────────────────────────────────────────────────

func launchInstances(ads []identity.AvailabilityDomain, accountName string) (sum, num int32) {
	adCount := int32(len(ads))
	adName := common.String(instance.AvailabilityDomain)
	sum = instance.Sum
	if sum <= 0 {
		sum = 1
	}

	// 是否固定可用性域
	adNotFixed := adName == nil || *adName == ""
	usableAds := make([]identity.AvailabilityDomain, len(ads))
	copy(usableAds, ads)

	// 实例名称
	name := instance.InstanceDisplayName
	if name == "" {
		name = time.Now().Format("instance-20060102-1504")
	}
	displayName := common.String(name)
	if sum > 1 {
		displayName = common.String(name + "-1")
	}

	// 构建创建请求
	request := core.LaunchInstanceRequest{}
	request.CompartmentId = common.String(oracle.Tenancy)
	request.DisplayName = displayName

	// 获取系统镜像
	fmt.Println("正在获取系统镜像...")
	image, err := getImage()
	if err != nil {
		printErr("获取系统镜像失败", err.Error())
		return
	}
	fmt.Println("系统镜像:", *image.DisplayName)

	// 获取 Shape
	var shape core.Shape
	if strings.Contains(strings.ToLower(instance.Shape), "flex") && instance.Ocpus > 0 && instance.MemoryInGBs > 0 {
		shape.Shape = &instance.Shape
		shape.Ocpus = &instance.Ocpus
		shape.MemoryInGBs = &instance.MemoryInGBs
	} else {
		fmt.Println("正在获取 Shape 信息...")
		shape, err = getShape(image.Id, instance.Shape)
		if err != nil {
			printErr("获取 Shape 失败", err.Error())
			return
		}
	}
	request.Shape = shape.Shape
	if strings.Contains(strings.ToLower(*shape.Shape), "flex") {
		request.ShapeConfig = &core.LaunchInstanceShapeConfigDetails{
			Ocpus:       shape.Ocpus,
			MemoryInGBs: shape.MemoryInGBs,
		}
		if instance.Burstable == "1/8" {
			request.ShapeConfig.BaselineOcpuUtilization = core.LaunchInstanceShapeConfigDetailsBaselineOcpuUtilization8
		} else if instance.Burstable == "1/2" {
			request.ShapeConfig.BaselineOcpuUtilization = core.LaunchInstanceShapeConfigDetailsBaselineOcpuUtilization2
		}
	}

	// 获取子网
	fmt.Println("正在获取子网...")
	subnet, err := createOrGetNetworkInfrastructure()
	if err != nil {
		printErr("获取子网失败", err.Error())
		return
	}
	fmt.Println("子网:", *subnet.DisplayName)
	request.CreateVnicDetails = &core.CreateVnicDetails{SubnetId: subnet.Id}

	// 引导卷
	sd := core.InstanceSourceViaImageDetails{ImageId: image.Id}
	if instance.BootVolumeSizeInGBs > 0 {
		sd.BootVolumeSizeInGBs = common.Int64(instance.BootVolumeSizeInGBs)
	}
	request.SourceDetails = sd
	request.IsPvEncryptionInTransitEnabled = common.Bool(true)

	// SSH 公钥 & cloud-init
	metaData := map[string]string{"ssh_authorized_keys": instance.SSH_Public_Key}
	if instance.CloudInit != "" {
		metaData["user_data"] = instance.CloudInit
	}
	request.Metadata = metaData

	// 打印启动信息
	bootVolumeSize := float64(instance.BootVolumeSizeInGBs)
	if bootVolumeSize == 0 {
		bootVolumeSize = math.Round(float64(*image.SizeInMBs) / 1024)
	}
	printf("\033[1;36m[%s] 开始创建 %s  OCPU: %g  内存: %g GB  引导卷: %g GB  数量: %d\033[0m\n",
		accountName, *shape.Shape, *shape.Ocpus, *shape.MemoryInGBs, bootVolumeSize, sum)

	// 循环创建
	retry := instance.Retry
	var failTimes, runTimes, adIndex int32
	var pos int32 = 0
	SKIP_RETRY_MAP := make(map[int32]bool)
	usableAdsTemp := make([]identity.AvailabilityDomain, 0)
	startTime := time.Now()

	for pos < sum {
		// 选择可用性域
		if adNotFixed {
			if int(adIndex) < len(usableAds) {
				adName = usableAds[adIndex].Name
				adIndex++
			} else {
				adIndex = 0
				if len(usableAds) > 0 {
					adName = usableAds[0].Name
					adIndex = 1
				}
			}
		}

		runTimes++
		printf("\033[1;36m[%s] 正在尝试创建第 %d 个实例  AD: %s  第 %d 次尝试\033[0m\n",
			accountName, pos+1, *adName, runTimes)
		request.AvailabilityDomain = adName
		createResp, err := computeClient.LaunchInstance(ctx, request)

		if err == nil {
			// ── 创建成功 ──
			num++
			duration := fmtDuration(time.Since(startTime))
			printf("\033[1;32m[%s] 第 %d 个实例抢到了🎉 正在启动...\033[0m\n", accountName, pos+1)

			// 等待并获取 IP（IPv4 + IPv6）
			ips, ipErr := getInstanceIPs(createResp.Instance.Id)
			if ipErr != nil {
				printf("\033[1;31m[%s] 实例启动失败: %s\033[0m\n", accountName, ipErr.Error())
				text := fmt.Sprintf("第%d个实例创建成功但启动失败❌\n区域:%s\n实例:%s\n配置:%s\n尝试:%d次\n耗时:%s",
					pos+1, oracle.Region, *createResp.Instance.DisplayName, *shape.Shape, runTimes, duration)
				sendMessage(accountName, text)
			} else {
				strIPv4 := strings.Join(ips.IPv4, ",")
				strIPv6 := strings.Join(ips.IPv6, ",")
				if strIPv4 == "" { strIPv4 = "无" }
				if strIPv6 == "" { strIPv6 = "无" }
				printf("\033[1;32m[%s] 第 %d 个实例启动成功✅  名称: %s  IPv4: %s  IPv6: %s\033[0m\n",
					accountName, pos+1, *createResp.Instance.DisplayName, strIPv4, strIPv6)
				text := fmt.Sprintf("第%d个实例抢到了🎉启动成功✅\n区域:%s\n实例:%s\nIPv4:%s\nIPv6:%s\nAD:%s\n配置:%s\nOCPU:%g 内存:%gGB 引导卷:%gGB\n尝试:%d次 耗时:%s",
					pos+1, oracle.Region, *createResp.Instance.DisplayName, strIPv4, strIPv6,
					*createResp.Instance.AvailabilityDomain, *shape.Shape,
					*shape.Ocpus, *shape.MemoryInGBs, bootVolumeSize, runTimes, duration)
				sendMessage(accountName, text)
			}

			sleepRandom(instance.MinTime, instance.MaxTime)
			displayName = common.String(fmt.Sprintf("%s-%d", name, pos+1))
			request.DisplayName = displayName
			failTimes = 0
			runTimes = 0
			adIndex = 0
			startTime = time.Now()
			pos++

		} else {
			// ── 创建失败 ──
			errInfo := err.Error()
			skipRetry := false
			servErr, isServErr := common.IsServiceError(err)
			if isServErr {
				errInfo = servErr.GetMessage()
			}

			// 判断是否可重试（4xx 客户端错误大多不可重试）
			if isServErr && ((400 <= servErr.GetHTTPStatusCode() && servErr.GetHTTPStatusCode() <= 405) ||
				(servErr.GetHTTPStatusCode() == 409 && !strings.EqualFold(servErr.GetCode(), "IncorrectState")) ||
				servErr.GetHTTPStatusCode() == 412 || servErr.GetHTTPStatusCode() == 422 ||
				servErr.GetHTTPStatusCode() == 431 || servErr.GetHTTPStatusCode() == 501) {
				skipRetry = true
				if adNotFixed {
					SKIP_RETRY_MAP[adIndex-1] = true
				}
				duration := fmtDuration(time.Since(startTime))
				printf("\033[1;31m[%s] 第 %d 个实例创建失败❌  错误: %s\033[0m\n", accountName, pos+1, errInfo)
				text := fmt.Sprintf("第%d个实例创建失败❌\n错误:%s\n区域:%s\n配置:%s\n尝试:%d次 耗时:%s",
					pos+1, errInfo, oracle.Region, *shape.Shape, runTimes, duration)
				sendMessage(accountName, text)
			} else {
				printf("\033[1;31m[%s] 创建失败(将重试): %s\033[0m\n", accountName, errInfo)
				if adNotFixed {
					SKIP_RETRY_MAP[adIndex-1] = false
				}
			}

			sleepRandom(instance.MinTime, instance.MaxTime)

			// 还没遍历完所有可用性域，继续下一个
			if adNotFixed && adIndex < adCount {
				continue
			}

			// 已遍历完一轮，统计可重试的域并判断是否继续
			failTimes++
			if adNotFixed {
				for idx, skip := range SKIP_RETRY_MAP {
					if !skip {
						usableAdsTemp = append(usableAdsTemp, usableAds[idx])
					}
				}
				usableAds = usableAdsTemp
				adCount = int32(len(usableAds))
				usableAdsTemp = nil
				for k := range SKIP_RETRY_MAP {
					delete(SKIP_RETRY_MAP, k)
				}
			}

			if (retry < 0 || failTimes <= retry) && !skipRetry && adCount > 0 {
				adIndex = 0
				continue
			}

			// 达到重试上限，跳过当前实例
			printf("\033[1;31m[%s] 第 %d 个实例放弃创建\033[0m\n", accountName, pos+1)
			// 重置
			usableAds = ads
			adCount = int32(len(usableAds))
			failTimes = 0
			runTimes = 0
			adIndex = 0
			startTime = time.Now()
			pos++
		}
	}
	return
}

// ── 网络基础设施 ──────────────────────────────────────────────────────────────────

func createOrGetNetworkInfrastructure() (subnet core.Subnet, err error) {
	vcn, err := createOrGetVcn()
	if err != nil {
		return
	}
	gateway, err := createOrGetInternetGateway(vcn.Id)
	if err != nil {
		return
	}
	_, err = createOrGetRouteTable(gateway.Id, vcn.Id)
	if err != nil {
		return
	}
	subnet, err = createOrGetSubnet(vcn.Id)
	return
}

func createOrGetVcn() (vcn core.Vcn, err error) {
	req := core.ListVcnsRequest{
		CompartmentId:   common.String(oracle.Tenancy),
		RequestMetadata: retryPolicy(),
	}
	resp, err := networkClient.ListVcns(ctx, req)
	if err != nil {
		return
	}
	displayName := instance.VcnDisplayName
	if len(resp.Items) > 0 && displayName == "" {
		return resp.Items[0], nil
	}
	for _, v := range resp.Items {
		if *v.DisplayName == displayName {
			return v, nil
		}
	}
	// 创建新 VCN（启用 IPv6）
	fmt.Println("开始创建VCN（启用IPv6）...")
	if displayName == "" {
		displayName = time.Now().Format("vcn-20060102-1504")
	}
	cr, err := networkClient.CreateVcn(ctx, core.CreateVcnRequest{
		CreateVcnDetails: core.CreateVcnDetails{
			CidrBlocks:    []string{"10.0.0.0/16"},
			CompartmentId: common.String(oracle.Tenancy),
			DisplayName:   common.String(displayName),
			DnsLabel:      common.String("vcndns"),
			// 让 OCI 自动分配一个 /56 的 Oracle 原生 IPv6 前缀
			IsIpv6Enabled: common.Bool(true),
		},
		RequestMetadata: retryPolicy(),
	})
	if err != nil {
		return
	}
	fmt.Printf("VCN创建成功: %s", *cr.Vcn.DisplayName)
	if len(cr.Vcn.Ipv6CidrBlocks) > 0 {
		fmt.Printf("  IPv6 CIDR: %s", strings.Join(cr.Vcn.Ipv6CidrBlocks, ", "))
	}
	fmt.Println()
	return cr.Vcn, nil
}

func createOrGetInternetGateway(vcnID *string) (gw core.InternetGateway, err error) {
	listResp, err := networkClient.ListInternetGateways(ctx, core.ListInternetGatewaysRequest{
		CompartmentId:   common.String(oracle.Tenancy),
		VcnId:           vcnID,
		RequestMetadata: retryPolicy(),
	})
	if err != nil {
		return
	}
	if len(listResp.Items) >= 1 {
		return listResp.Items[0], nil
	}
	fmt.Println("开始创建Internet网关...")
	enabled := true
	cr, err := networkClient.CreateInternetGateway(ctx, core.CreateInternetGatewayRequest{
		CreateInternetGatewayDetails: core.CreateInternetGatewayDetails{
			CompartmentId: common.String(oracle.Tenancy),
			IsEnabled:     &enabled,
			VcnId:         vcnID,
		},
		RequestMetadata: retryPolicy(),
	})
	if err != nil {
		return
	}
	fmt.Printf("Internet网关创建成功: %s\n", *cr.InternetGateway.DisplayName)
	return cr.InternetGateway, nil
}

func createOrGetRouteTable(gatewayID, vcnID *string) (rt core.RouteTable, err error) {
	listResp, err := networkClient.ListRouteTables(ctx, core.ListRouteTablesRequest{
		CompartmentId:   common.String(oracle.Tenancy),
		VcnId:           vcnID,
		RequestMetadata: retryPolicy(),
	})
	if err != nil {
		return
	}
	cidr := "0.0.0.0/0"
	rr := core.RouteRule{
		NetworkEntityId: gatewayID,
		Destination:     &cidr,
		DestinationType: core.RouteRuleDestinationTypeCidrBlock,
	}
	if len(listResp.Items) >= 1 {
		if len(listResp.Items[0].RouteRules) >= 1 {
			return listResp.Items[0], nil
		}
		fmt.Println("路由表未配置规则，添加Internet路由规则...")
		ur, uerr := networkClient.UpdateRouteTable(ctx, core.UpdateRouteTableRequest{
			RtId: listResp.Items[0].Id,
			UpdateRouteTableDetails: core.UpdateRouteTableDetails{
				RouteRules: []core.RouteRule{rr},
			},
			RequestMetadata: retryPolicy(),
		})
		if uerr != nil {
			return rt, uerr
		}
		fmt.Println("路由规则添加成功")
		return ur.RouteTable, nil
	}
	return rt, errors.New("未找到默认路由表")
}

func createOrGetSubnet(vcnID *string) (subnet core.Subnet, err error) {
	listResp, err := networkClient.ListSubnets(ctx, core.ListSubnetsRequest{
		CompartmentId:   common.String(oracle.Tenancy),
		VcnId:           vcnID,
		RequestMetadata: retryPolicy(),
	})
	if err != nil {
		return
	}
	displayName := instance.SubnetDisplayName
	if len(listResp.Items) > 0 && displayName == "" {
		return listResp.Items[0], nil
	}
	for _, s := range listResp.Items {
		if *s.DisplayName == displayName {
			return s, nil
		}
	}
	// 创建子网（启用 IPv6，自动从 VCN IPv6 前缀分配 /64）
	fmt.Println("开始创建Subnet（启用IPv6）...")
	if displayName == "" {
		displayName = time.Now().Format("subnet-20060102-1504")
	}

	// 先获取 VCN 的 IPv6 前缀，用于子网分配
	vcnResp, vcnErr := networkClient.GetVcn(ctx, core.GetVcnRequest{
		VcnId:           vcnID,
		RequestMetadata: retryPolicy(),
	})
	subnetDetails := core.CreateSubnetDetails{
		CompartmentId: common.String(oracle.Tenancy),
		VcnId:         vcnID,
		CidrBlock:     common.String("10.0.0.0/20"),
		DisplayName:   common.String(displayName),
		DnsLabel:      common.String("subnetdns"),
	}
	// 如果 VCN 已有 IPv6 前缀，则为子网申请一个 /64
	if vcnErr == nil && len(vcnResp.Vcn.Ipv6CidrBlocks) > 0 {
		subnetDetails.Ipv6CidrBlocks = []string{} // 空列表触发 OCI 自动从 VCN 前缀分配 /64
	}

	cr, err := networkClient.CreateSubnet(ctx, core.CreateSubnetRequest{
		CreateSubnetDetails: subnetDetails,
		RequestMetadata:     retryPolicy(),
	})
	if err != nil {
		return
	}

	// 更新安全列表：允许所有 IPv4 和 IPv6 入站流量
	getResp, err := networkClient.GetSecurityList(ctx, core.GetSecurityListRequest{
		SecurityListId:  common.String(cr.SecurityListIds[0]),
		RequestMetadata: retryPolicy(),
	})
	if err == nil {
		newRules := append(getResp.IngressSecurityRules,
			// IPv4 全放行
			core.IngressSecurityRule{
				Protocol: common.String("all"),
				Source:   common.String("0.0.0.0/0"),
			},
			// IPv6 全放行
			core.IngressSecurityRule{
				Protocol: common.String("all"),
				Source:   common.String("::/0"),
			},
		)
		networkClient.UpdateSecurityList(ctx, core.UpdateSecurityListRequest{
			SecurityListId: common.String(cr.SecurityListIds[0]),
			UpdateSecurityListDetails: core.UpdateSecurityListDetails{
				IngressSecurityRules: newRules,
			},
			RequestMetadata: retryPolicy(),
		})
	}

	fmt.Printf("Subnet创建成功: %s", *cr.Subnet.DisplayName)
	if len(cr.Subnet.Ipv6CidrBlocks) > 0 {
		fmt.Printf("  IPv6 CIDR: %s", strings.Join(cr.Subnet.Ipv6CidrBlocks, ", "))
	}
	fmt.Println()
	return cr.Subnet, nil
}

// ── 镜像 & Shape ──────────────────────────────────────────────────────────────────

func getImage() (image core.Image, err error) {
	if instance.OperatingSystem == "" || instance.OperatingSystemVersion == "" {
		return image, errors.New("操作系统类型和版本不能为空")
	}
	r, err := computeClient.ListImages(ctx, core.ListImagesRequest{
		CompartmentId:          common.String(oracle.Tenancy),
		OperatingSystem:        common.String(instance.OperatingSystem),
		OperatingSystemVersion: common.String(instance.OperatingSystemVersion),
		Shape:                  common.String(instance.Shape),
		RequestMetadata:        retryPolicy(),
	})
	if err != nil {
		return
	}
	if len(r.Items) == 0 {
		return image, fmt.Errorf("未找到 [%s %s] 的镜像", instance.OperatingSystem, instance.OperatingSystemVersion)
	}
	return r.Items[0], nil
}

func getShape(imageId *string, shapeName string) (core.Shape, error) {
	r, err := computeClient.ListShapes(ctx, core.ListShapesRequest{
		CompartmentId:   common.String(oracle.Tenancy),
		ImageId:         imageId,
		RequestMetadata: retryPolicy(),
	})
	if err != nil {
		return core.Shape{}, err
	}
	for _, s := range r.Items {
		if strings.EqualFold(*s.Shape, shapeName) {
			return s, nil
		}
	}
	return core.Shape{}, errors.New("没有符合条件的Shape")
}

// ── 获取实例公共 IP (IPv4 + IPv6) ─────────────────────────────────────────────────

type InstanceIPs struct {
	IPv4 []string
	IPv6 []string
}

func getInstanceIPs(instanceId *string) (InstanceIPs, error) {
	result := InstanceIPs{}

	// 等待实例 Running（最多 10 分钟）
	for i := 0; i < 60; i++ {
		resp, err := computeClient.GetInstance(ctx, core.GetInstanceRequest{
			InstanceId:      instanceId,
			RequestMetadata: retryPolicy(),
		})
		if err != nil {
			return result, err
		}
		if resp.LifecycleState == core.InstanceLifecycleStateRunning {
			break
		}
		if resp.LifecycleState == core.InstanceLifecycleStateTerminated ||
			resp.LifecycleState == core.InstanceLifecycleStateTerminating {
			return result, errors.New("实例已终止")
		}
		time.Sleep(10 * time.Second)
	}

	// 获取 VNIC 附件列表
	vasResp, err := computeClient.ListVnicAttachments(ctx, core.ListVnicAttachmentsRequest{
		CompartmentId:   common.String(oracle.Tenancy),
		InstanceId:      instanceId,
		RequestMetadata: retryPolicy(),
	})
	if err != nil {
		return result, err
	}

	for _, va := range vasResp.Items {
		// 获取 VNIC 基本信息（含 IPv4 PublicIp）
		vnicResp, err := networkClient.GetVnic(ctx, core.GetVnicRequest{
			VnicId:          va.VnicId,
			RequestMetadata: retryPolicy(),
		})
		if err != nil {
			continue
		}
		if vnicResp.PublicIp != nil && *vnicResp.PublicIp != "" {
			result.IPv4 = append(result.IPv4, *vnicResp.PublicIp)
		}

		// 获取该 VNIC 的 IPv6 地址列表
		ipv6Resp, err := networkClient.ListIpv6s(ctx, core.ListIpv6sRequest{
			VnicId:          va.VnicId,
			RequestMetadata: retryPolicy(),
		})
		if err == nil {
			for _, ipv6 := range ipv6Resp.Items {
				if ipv6.IpAddress != nil && *ipv6.IpAddress != "" {
					result.IPv6 = append(result.IPv6, *ipv6.IpAddress)
				}
			}
		}
	}

	if len(result.IPv4) == 0 && len(result.IPv6) == 0 {
		return result, errors.New("未获取到任何公共IP")
	}
	return result, nil
}

// ── 可用性域 ──────────────────────────────────────────────────────────────────────

func listAvailabilityDomains() ([]identity.AvailabilityDomain, error) {
	resp, err := identityClient.ListAvailabilityDomains(ctx, identity.ListAvailabilityDomainsRequest{
		CompartmentId:   common.String(oracle.Tenancy),
		RequestMetadata: retryPolicy(),
	})
	return resp.Items, err
}

// ── Telegram 消息 ─────────────────────────────────────────────────────────────────

func sendMessage(name, text string) {
	if token == "" || chat_id == "" {
		return
	}
	data := url.Values{
		"parse_mode": {"Markdown"},
		"chat_id":    {chat_id},
		"text":       {"🔰*甲骨文通知* " + name + "\n" + text},
	}
	req, err := http.NewRequest(http.MethodPost, sendMessageUrl, strings.NewReader(data.Encode()))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{}
	if proxy != "" {
		proxyURL, _ := url.Parse(proxy)
		client = &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	}
	client.Do(req)
}

// ── 工具函数 ──────────────────────────────────────────────────────────────────────

func getProvider(o Oracle) (common.ConfigurationProvider, error) {
	content, err := ioutil.ReadFile(o.Key_file)
	if err != nil {
		return nil, err
	}
	passphrase := common.String(o.Key_password)
	return common.NewRawConfigurationProvider(o.Tenancy, o.User, o.Region, o.Fingerprint, string(content), passphrase), nil
}

func setProxyOrNot(client *common.BaseClient) {
	if proxy == "" {
		return
	}
	proxyURL, err := url.Parse(proxy)
	if err != nil {
		return
	}
	client.HTTPClient = &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
	}
}

func retryPolicy() common.RequestMetadata {
	attempts := uint(3)
	policy := common.NewRetryPolicyWithOptions(
		common.WithConditionalOption(true, common.ReplaceWithValuesFromRetryPolicy(common.DefaultRetryPolicyWithoutEventualConsistency())),
		common.WithMaximumNumberAttempts(attempts),
		common.WithShouldRetryOperation(func(r common.OCIOperationResponse) bool {
			return !(r.Error == nil && 199 < r.Response.HTTPResponse().StatusCode && r.Response.HTTPResponse().StatusCode < 300)
		}),
	)
	return common.RequestMetadata{RetryPolicy: &policy}
}

func sleepRandom(min, max int32) {
	var second int32
	if min <= 0 || max <= 0 {
		second = 1
	} else if min >= max {
		second = max
	} else {
		second = rand.Int31n(max-min) + min
	}
	printf("Sleep %d 秒...\n", second)
	time.Sleep(time.Duration(second) * time.Second)
}

func fmtDuration(d time.Duration) string {
	days := int(d / (time.Hour * 24))
	hours := int((d % (time.Hour * 24)).Hours())
	minutes := int((d % time.Hour).Minutes())
	seconds := int((d % time.Minute).Seconds())
	var parts []string
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%d天", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%d时", hours))
	}
	if minutes > 0 {
		parts = append(parts, fmt.Sprintf("%d分", minutes))
	}
	if seconds > 0 {
		parts = append(parts, fmt.Sprintf("%d秒", seconds))
	}
	if len(parts) == 0 {
		return "< 1秒"
	}
	return strings.Join(parts, " ")
}

func printf(format string, a ...interface{}) {
	fmt.Printf("%s ", time.Now().Format("2006-01-02 15:04:05"))
	fmt.Printf(format, a...)
}

func printErr(desc, detail string) {
	fmt.Printf("\033[1;31mError: %s. %s\033[0m\n", desc, detail)
}
