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
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/core"
	"github.com/oracle/oci-go-sdk/v65/identity"
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
	chatID  string

	sendMessageURL string
)

// ── 数据结构 ────────────────────────────────────────────────────────────────────

type Oracle struct {
	User         string `ini:"user"`
	Fingerprint  string `ini:"fingerprint"`
	Tenancy      string `ini:"tenancy"`
	Region       string `ini:"region"`
	KeyFile      string `ini:"key_file"`
	KeyPassword  string `ini:"key_password"`
}

type Instance struct {
	AvailabilityDomain     string  `ini:"availabilityDomain"`
	SSHPublicKey           string  `ini:"ssh_authorized_key"`
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
	EnableIPv6             bool    `ini:"enableIPv6"`
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
		printUsage()
		return
	}

	// 读取配置文件
	cfg, err := ini.Load(configFilePath)
	if err != nil {
		printError("读取配置文件失败", err)
		return
	}

	// 加载全局配置
	loadGlobalConfig(cfg)

	// 找到第一个有效的甲骨文账号配置
	oracleSection := findValidOracleSection(cfg)
	if oracleSection == nil {
		printError("未找到有效的甲骨文账号配置", nil)
		return
	}

	// 初始化 Oracle 客户端
	if err := initOracleClient(oracleSection); err != nil {
		printError("初始化 Oracle 客户端失败", err)
		return
	}

	// 确定使用 ARM 还是 AMD 的实例配置段
	instanceSectionName := "INSTANCE.ARM"
	if useAMD {
		instanceSectionName = "INSTANCE.AMD"
	}

	// 加载实例配置
	if err := loadInstanceConfig(cfg, instanceSectionName); err != nil {
		printError("加载实例配置失败", err)
		return
	}

	// 获取可用性域
	fmt.Println("正在获取可用性域...")
	ads, err := listAvailabilityDomains()
	if err != nil {
		printError("获取可用性域失败", err)
		return
	}

	// 开始创建实例
	fmt.Printf("\033[1;36m[%s] 账号: %s 开始创建实例...\033[0m\n", 
		instanceSectionName, oracleSection.Name())
	
	success, total := launchInstances(ads, oracleSection.Name())
	fmt.Printf("\033[1;36m创建完成。总数: %d 成功: %d 失败: %d\033[0m\n", 
		total, success, total-success)
}

func printUsage() {
	fmt.Println("请指定 -arm 或 -amd 参数")
	fmt.Println("  -arm   创建 VM.Standard.A1.Flex")
	fmt.Println("  -amd   创建 VM.Standard.E2.1.Micro")
}

// ── 配置加载 ────────────────────────────────────────────────────────────────────

func loadGlobalConfig(cfg *ini.File) {
	defSec := cfg.Section(ini.DefaultSection)
	proxy = defSec.Key("proxy").Value()
	token = defSec.Key("token").Value()
	chatID = defSec.Key("chat_id").Value()
	sendMessageURL = "https://api.telegram.org/bot" + token + "/sendMessage"
	rand.Seed(time.Now().UnixNano())
}

func findValidOracleSection(cfg *ini.File) *ini.Section {
	for _, sec := range cfg.Sections() {
		if len(sec.ParentKeys()) == 0 {
			user := sec.Key("user").Value()
			fingerprint := sec.Key("fingerprint").Value()
			tenancy := sec.Key("tenancy").Value()
			region := sec.Key("region").Value()
			keyFile := sec.Key("key_file").Value()
			if user != "" && fingerprint != "" && tenancy != "" && region != "" && keyFile != "" {
				return sec
			}
		}
	}
	return nil
}

func initOracleClient(section *ini.Section) error {
	oracle = Oracle{}
	if err := section.MapTo(&oracle); err != nil {
		return fmt.Errorf("解析账号配置失败: %w", err)
	}

	provider = common.NewRawConfigurationProvider(
		oracle.Tenancy,
		oracle.User,
		oracle.Region,
		oracle.Fingerprint,
		getPrivateKey(oracle.KeyFile),
		common.String(oracle.KeyPassword),
	)

	var err error
	computeClient, err = core.NewComputeClientWithConfigurationProvider(provider)
	if err != nil {
		return fmt.Errorf("创建 ComputeClient 失败: %w", err)
	}
	setProxy(&computeClient.BaseClient)

	networkClient, err = core.NewVirtualNetworkClientWithConfigurationProvider(provider)
	if err != nil {
		return fmt.Errorf("创建 VirtualNetworkClient 失败: %w", err)
	}
	setProxy(&networkClient.BaseClient)

	identityClient, err = identity.NewIdentityClientWithConfigurationProvider(provider)
	if err != nil {
		return fmt.Errorf("创建 IdentityClient 失败: %w", err)
	}
	setProxy(&identityClient.BaseClient)

	return nil
}

func getPrivateKey(path string) string {
	content, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(content)
}

func loadInstanceConfig(cfg *ini.File, instanceSectionName string) error {
	instance = Instance{}
	
	// 加载基础配置
	baseSection, err := cfg.GetSection("INSTANCE")
	if err == nil {
		if err := baseSection.MapTo(&instance); err != nil {
			return fmt.Errorf("解析 [INSTANCE] 配置失败: %w", err)
		}
	}

	// 加载特定配置
	instanceSection, err := cfg.GetSection(instanceSectionName)
	if err != nil {
		return fmt.Errorf("未找到配置段 [%s]", instanceSectionName)
	}
	
	if err := instanceSection.MapTo(&instance); err != nil {
		return fmt.Errorf("解析 [%s] 配置失败: %w", instanceSectionName, err)
	}

	return nil
}

// ── 创建实例核心逻辑 ──────────────────────────────────────────────────────────────

func launchInstances(ads []identity.AvailabilityDomain, accountName string) (success, total int32) {
	adCount := int32(len(ads))
	adName := instance.AvailabilityDomain
	total = instance.Sum
	if total <= 0 {
		total = 1
	}

	// 是否固定可用性域
	adNotFixed := adName == ""
	usableAds := make([]identity.AvailabilityDomain, len(ads))
	copy(usableAds, ads)

	// 实例名称
	name := instance.InstanceDisplayName
	if name == "" {
		name = time.Now().Format("instance-20060102-1504")
	}
	displayName := name
	if total > 1 {
		displayName = name + "-1"
	}

	// 获取系统镜像
	fmt.Println("正在获取系统镜像...")
	image, err := getImage()
	if err != nil {
		printError("获取系统镜像失败", err)
		return
	}
	fmt.Printf("系统镜像: %s\n", *image.DisplayName)

	// 准备创建请求
	request, err := prepareLaunchRequest(image, displayName)
	if err != nil {
		printError("准备创建请求失败", err)
		return
	}

	// 获取子网
	fmt.Println("正在获取子网...")
	subnet, err := createOrGetNetworkInfrastructure()
	if err != nil {
		printError("获取子网失败", err)
		return
	}
	fmt.Printf("子网: %s\n", *subnet.DisplayName)
	request.CreateVnicDetails = &core.CreateVnicDetails{SubnetId: subnet.Id}

	// 打印启动信息
	printLaunchInfo(&image, accountName)

	// 发送开始创建通知
	{
		ocpus := instance.Ocpus
		memory := instance.MemoryInGBs
		if ocpus == 0 { ocpus = 1 }
		if memory == 0 { memory = 1 }
		bootVolumeSize := float64(instance.BootVolumeSizeInGBs)
		if bootVolumeSize == 0 {
			bootVolumeSize = math.Round(float64(*image.SizeInMBs) / 1024)
		}
		text := fmt.Sprintf("开始抢注实例🚀\n区域:%s\n配置:%s\nOCPU:%g 内存:%gGB 引导卷:%gGB\n数量:%d",
			oracle.Region, instance.Shape, ocpus, memory, bootVolumeSize, total)
		sendMessage(accountName, text)
	}

	// 循环创建实例
	var failTimes, runTimes, adIndex int32
	var pos int32 = 0
	skipRetryMap := make(map[int32]bool)
	startTime := time.Now()

	for pos < total {
		// 选择可用性域
		if adNotFixed && len(usableAds) > 0 {
			adIndex = adIndex % int32(len(usableAds))
			adName = *usableAds[adIndex].Name
			adIndex++
		}

		runTimes++
		logPrintf(accountName, "正在尝试创建第 %d 个实例 AD: %s 第 %d 次尝试", 
			pos+1, adName, runTimes)
		
		request.AvailabilityDomain = common.String(adName)
		createResp, err := computeClient.LaunchInstance(ctx, request)

		if err == nil {
			// 创建成功
			success++
			duration := formatDuration(time.Since(startTime))
			logPrintf(accountName, "第 %d 个实例创建成功🎉 正在启动...", pos+1)

			// 等待并获取 IP
			handleSuccessInstance(&createResp.Instance, accountName, pos+1, 
				runTimes, duration, &image, &displayName, name)

			sleepRandom(instance.MinTime, instance.MaxTime)
			displayName = fmt.Sprintf("%s-%d", name, pos+1)
			request.DisplayName = common.String(displayName)
			failTimes = 0
			runTimes = 0
			adIndex = 0
			startTime = time.Now()
			pos++
		} else {
			// 创建失败
			skipRetry := handleFailure(err, accountName, pos+1, runTimes, 
				&startTime, adNotFixed, &adIndex, skipRetryMap)

			sleepRandom(instance.MinTime, instance.MaxTime)

			// 处理重试逻辑
			if shouldContinueRetry(adNotFixed, adIndex, adCount, failTimes, 
				skipRetry, &usableAds, &skipRetryMap, &adCount) {
				continue
			}

			// 增加失败次数计数
			failTimes++
			
			// 判断是否应该放弃
			shouldGiveUp := false
			if instance.Retry == -1 {
				// retry = -1 表示永远重试，但如果是不可重试错误则放弃
				shouldGiveUp = skipRetry
				if !skipRetry {
					logPrintf(accountName, "第 %d 个实例继续重试 (retry=-1, 失败次数: %d)", pos+1, failTimes)
				}
			} else {
				// 达到重试次数上限则放弃
				shouldGiveUp = failTimes > instance.Retry || skipRetry
			}
			
			if shouldGiveUp {
				// 放弃当前实例
				logPrintf(accountName, "第 %d 个实例放弃创建 (失败次数: %d)", pos+1, failTimes)
				resetForNextInstance(ads, &usableAds, &adCount, &failTimes, 
					&runTimes, &adIndex, &startTime)
				pos++
			} else {
				// 继续重试
				adIndex = 0
				continue
			}
		}
	}
	return
}

func prepareLaunchRequest(image core.Image, displayName string) (core.LaunchInstanceRequest, error) {
	request := core.LaunchInstanceRequest{}
	request.CompartmentId = common.String(oracle.Tenancy)
	request.DisplayName = common.String(displayName)

	// 获取 Shape 配置
	if strings.Contains(strings.ToLower(instance.Shape), "flex") && 
		instance.Ocpus > 0 && instance.MemoryInGBs > 0 {
		request.Shape = common.String(instance.Shape)
		request.ShapeConfig = &core.LaunchInstanceShapeConfigDetails{
			Ocpus:       common.Float32(instance.Ocpus),
			MemoryInGBs: common.Float32(instance.MemoryInGBs),
		}
		setBurstableConfig(request.ShapeConfig)
	} else {
		shape, err := getShape(image.Id, instance.Shape)
		if err != nil {
			return request, fmt.Errorf("获取 Shape 失败: %w", err)
		}
		request.Shape = shape.Shape
		if shape.Ocpus != nil && shape.MemoryInGBs != nil {
			request.ShapeConfig = &core.LaunchInstanceShapeConfigDetails{
				Ocpus:       shape.Ocpus,
				MemoryInGBs: shape.MemoryInGBs,
			}
		}
	}

	// 配置引导卷
	sourceDetails := core.InstanceSourceViaImageDetails{
		ImageId: image.Id,
	}
	if instance.BootVolumeSizeInGBs > 0 {
		sourceDetails.BootVolumeSizeInGBs = common.Int64(instance.BootVolumeSizeInGBs)
	}
	request.SourceDetails = sourceDetails
	request.IsPvEncryptionInTransitEnabled = common.Bool(true)

	// 配置元数据
	metadata := map[string]string{"ssh_authorized_keys": instance.SSHPublicKey}
	if instance.CloudInit != "" {
		metadata["user_data"] = instance.CloudInit
	}
	request.Metadata = metadata

	return request, nil
}

func setBurstableConfig(config *core.LaunchInstanceShapeConfigDetails) {
	switch instance.Burstable {
	case "1/8":
		config.BaselineOcpuUtilization = core.LaunchInstanceShapeConfigDetailsBaselineOcpuUtilization8
	case "1/2":
		config.BaselineOcpuUtilization = core.LaunchInstanceShapeConfigDetailsBaselineOcpuUtilization2
	}
}

func getShape(imageID *string, shapeName string) (core.Shape, error) {
	resp, err := computeClient.ListShapes(ctx, core.ListShapesRequest{
		CompartmentId:   common.String(oracle.Tenancy),
		ImageId:         imageID,
		RequestMetadata: getRetryPolicy(),
	})
	if err != nil {
		return core.Shape{}, err
	}
	
	for _, shape := range resp.Items {
		if strings.EqualFold(*shape.Shape, shapeName) {
			return shape, nil
		}
	}
	return core.Shape{}, errors.New("没有符合条件的 Shape")
}

func printLaunchInfo(image *core.Image, accountName string) {
	bootVolumeSize := float64(instance.BootVolumeSizeInGBs)
	if bootVolumeSize == 0 {
		bootVolumeSize = math.Round(float64(*image.SizeInMBs) / 1024)
	}
	
	ocpus := instance.Ocpus
	memory := instance.MemoryInGBs
	if ocpus == 0 {
		ocpus = 1
	}
	if memory == 0 {
		memory = 1
	}
	
	logPrintf(accountName, "开始创建 %s OCPU: %g 内存: %g GB 引导卷: %g GB 数量: %d",
		instance.Shape, ocpus, memory, bootVolumeSize, instance.Sum)
}

func handleSuccessInstance(inst *core.Instance, accountName string, pos, runTimes int32,
	duration string, image *core.Image, displayName *string, baseName string) {
	
	ips, err := getInstanceIPs(inst.Id)
	if err != nil {
		logPrintf(accountName, "实例启动失败: %v", err)
		text := fmt.Sprintf("第%d个实例创建成功但启动失败❌\n区域:%s\n实例:%s\n配置:%s\n尝试:%d次\n耗时:%s",
			pos, oracle.Region, *inst.DisplayName, instance.Shape, runTimes, duration)
		sendMessage(accountName, text)
		return
	}

	ipv4Str := "无"
	ipv6Str := "无"
	if len(ips.IPv4) > 0 {
		ipv4Str = strings.Join(ips.IPv4, ",")
	}
	if len(ips.IPv6) > 0 {
		ipv6Str = strings.Join(ips.IPv6, ",")
	}

	logPrintf(accountName, "第 %d 个实例启动成功✅ 名称: %s IPv4: %s IPv6: %s",
		pos, *inst.DisplayName, ipv4Str, ipv6Str)

	bootVolumeSize := float64(instance.BootVolumeSizeInGBs)
	if bootVolumeSize == 0 {
		bootVolumeSize = math.Round(float64(*image.SizeInMBs) / 1024)
	}

	text := fmt.Sprintf("第%d个实例创建成功🎉启动成功✅\n区域:%s\n实例:%s\nIPv4:%s\nIPv6:%s\nAD:%s\n配置:%s\nOCPU:%g 内存:%gGB 引导卷:%gGB\n尝试:%d次 耗时:%s",
		pos, oracle.Region, *inst.DisplayName, ipv4Str, ipv6Str,
		*inst.AvailabilityDomain, instance.Shape,
		instance.Ocpus, instance.MemoryInGBs, bootVolumeSize, runTimes, duration)
	sendMessage(accountName, text)
}

func handleFailure(err error, accountName string, pos, runTimes int32, 
	startTime *time.Time, adNotFixed bool, adIndex *int32, 
	skipRetryMap map[int32]bool) bool {
	
	errInfo := err.Error()
	skipRetry := false
	
	if serviceErr, ok := common.IsServiceError(err); ok {
		errInfo = serviceErr.GetMessage()
		
		// 获取HTTP状态码和错误码
		statusCode := serviceErr.GetHTTPStatusCode()
		errorCode := serviceErr.GetCode()
		
		// 判断是否应该放弃重试
		// 只有明确的客户端错误才放弃重试，服务限制错误应该继续重试
		if (400 <= statusCode && statusCode <= 405) ||
			(statusCode == 409 && !strings.EqualFold(errorCode, "IncorrectState") && 
			 !strings.Contains(strings.ToLower(errInfo), "limit")) || // 排除限制错误
			statusCode == 412 || statusCode == 422 ||
			statusCode == 431 || statusCode == 501 {
			
			// 检查是否包含"limit"关键词，限制类错误应该重试
			if strings.Contains(strings.ToLower(errInfo), "limit") ||
			   strings.Contains(strings.ToLower(errInfo), "quota") ||
			   strings.Contains(strings.ToLower(errInfo), "exceeded") {
				// 这是限制错误，应该重试
				skipRetry = false
				logPrintf(accountName, "遇到服务限制(将重试): %s", errInfo)
			} else {
				// 真正的不可重试错误
				skipRetry = true
				if adNotFixed {
					skipRetryMap[*adIndex-1] = true
				}
				duration := formatDuration(time.Since(*startTime))
				logPrintf(accountName, "第 %d 个实例创建失败❌ 错误: %s", pos, errInfo)
				text := fmt.Sprintf("第%d个实例创建失败❌\n错误:%s\n区域:%s\n配置:%s\n尝试:%d次 耗时:%s",
					pos, errInfo, oracle.Region, instance.Shape, runTimes, duration)
				sendMessage(accountName, text)
			}
		} else {
			logPrintf(accountName, "创建失败(将重试): %s", errInfo)
			if adNotFixed {
				skipRetryMap[*adIndex-1] = false
			}
		}
	} else {
		logPrintf(accountName, "创建失败(将重试): %s", errInfo)
	}
	
	return skipRetry
}

func shouldContinueRetry(adNotFixed bool, adIndex, adCount, failTimes int32,
	skipRetry bool, usableAds *[]identity.AvailabilityDomain,
	skipRetryMap *map[int32]bool, adCountPtr *int32) bool {
	
	// 还没遍历完所有可用性域，继续下一个
	if adNotFixed && adIndex < adCount {
		return true
	}

	// 已遍历完一轮，更新可用域列表
	if adNotFixed {
		updateUsableAds(usableAds, *skipRetryMap)
		*adCountPtr = int32(len(*usableAds))
		*skipRetryMap = make(map[int32]bool)
	}

	// 如果设置了永远重试（retry = -1），并且不是不可重试错误，则继续
	if instance.Retry == -1 && !skipRetry && *adCountPtr > 0 {
		return true
	}

	return false
}

func updateUsableAds(usableAds *[]identity.AvailabilityDomain, skipRetryMap map[int32]bool) {
	if len(skipRetryMap) == 0 {
		return
	}
	
	newAds := make([]identity.AvailabilityDomain, 0)
	for idx, ad := range *usableAds {
		if !skipRetryMap[int32(idx)] {
			newAds = append(newAds, ad)
		}
	}
	*usableAds = newAds
}

func resetForNextInstance(ads []identity.AvailabilityDomain, 
	usableAds *[]identity.AvailabilityDomain, adCount *int32,
	failTimes, runTimes, adIndex *int32, startTime *time.Time) {
	
	*usableAds = make([]identity.AvailabilityDomain, len(ads))
	copy(*usableAds, ads)
	*adCount = int32(len(*usableAds))
	*failTimes = 0
	*runTimes = 0
	*adIndex = 0
	*startTime = time.Now()
}

// ── 网络基础设施 ──────────────────────────────────────────────────────────────────

func createOrGetNetworkInfrastructure() (core.Subnet, error) {
	vcn, err := createOrGetVcn()
	if err != nil {
		return core.Subnet{}, err
	}
	
	gateway, err := createOrGetInternetGateway(vcn.Id)
	if err != nil {
		return core.Subnet{}, err
	}
	
	_, err = createOrGetRouteTable(gateway.Id, vcn.Id)
	if err != nil {
		return core.Subnet{}, err
	}
	
	return createOrGetSubnet(vcn.Id)
}

func createOrGetVcn() (core.Vcn, error) {
	resp, err := networkClient.ListVcns(ctx, core.ListVcnsRequest{
		CompartmentId:   common.String(oracle.Tenancy),
		RequestMetadata: getRetryPolicy(),
	})
	if err != nil {
		return core.Vcn{}, err
	}

	displayName := instance.VcnDisplayName
	if len(resp.Items) > 0 && displayName == "" {
		return resp.Items[0], nil
	}

	for _, vcn := range resp.Items {
		if *vcn.DisplayName == displayName {
			return vcn, nil
		}
	}

	// 创建新 VCN
	fmt.Println("开始创建 VCN...")
	if displayName == "" {
		displayName = time.Now().Format("vcn-20060102-1504")
	}

	createDetails := core.CreateVcnDetails{
		CidrBlocks:    []string{"10.0.0.0/16"},
		CompartmentId: common.String(oracle.Tenancy),
		DisplayName:   common.String(displayName),
		DnsLabel:      common.String("vcndns"),
	}

	// 如果需要 IPv6
	if instance.EnableIPv6 {
		createDetails.IsIpv6Enabled = common.Bool(true)
	}

	createResp, err := networkClient.CreateVcn(ctx, core.CreateVcnRequest{
		CreateVcnDetails: createDetails,
		RequestMetadata:  getRetryPolicy(),
	})
	
	if err != nil {
		return core.Vcn{}, err
	}

	fmt.Printf("VCN 创建成功: %s", *createResp.Vcn.DisplayName)
	if instance.EnableIPv6 && len(createResp.Vcn.Ipv6CidrBlocks) > 0 {
		fmt.Printf(" IPv6 CIDR: %s", strings.Join(createResp.Vcn.Ipv6CidrBlocks, ", "))
	}
	fmt.Println()
	
	return createResp.Vcn, nil
}

func createOrGetInternetGateway(vcnID *string) (core.InternetGateway, error) {
	resp, err := networkClient.ListInternetGateways(ctx, core.ListInternetGatewaysRequest{
		CompartmentId:   common.String(oracle.Tenancy),
		VcnId:           vcnID,
		RequestMetadata: getRetryPolicy(),
	})
	if err != nil {
		return core.InternetGateway{}, err
	}

	if len(resp.Items) >= 1 {
		return resp.Items[0], nil
	}

	fmt.Println("开始创建 Internet 网关...")
	enabled := true
	createResp, err := networkClient.CreateInternetGateway(ctx, core.CreateInternetGatewayRequest{
		CreateInternetGatewayDetails: core.CreateInternetGatewayDetails{
			CompartmentId: common.String(oracle.Tenancy),
			IsEnabled:     &enabled,
			VcnId:         vcnID,
			DisplayName:   common.String("internet-gateway-" + time.Now().Format("20060102")),
		},
		RequestMetadata: getRetryPolicy(),
	})
	
	if err != nil {
		return core.InternetGateway{}, err
	}

	fmt.Printf("Internet 网关创建成功: %s\n", *createResp.InternetGateway.DisplayName)
	return createResp.InternetGateway, nil
}

func createOrGetRouteTable(gatewayID, vcnID *string) (core.RouteTable, error) {
	resp, err := networkClient.ListRouteTables(ctx, core.ListRouteTablesRequest{
		CompartmentId:   common.String(oracle.Tenancy),
		VcnId:           vcnID,
		RequestMetadata: getRetryPolicy(),
	})
	if err != nil {
		return core.RouteTable{}, err
	}

	if len(resp.Items) == 0 {
		return core.RouteTable{}, errors.New("未找到路由表")
	}

	routeTable := resp.Items[0]
	cidr := "0.0.0.0/0"
	routeRule := core.RouteRule{
		NetworkEntityId: gatewayID,
		Destination:     &cidr,
		DestinationType: core.RouteRuleDestinationTypeCidrBlock,
	}

	// 检查是否需要更新路由规则
	if len(routeTable.RouteRules) == 0 {
		fmt.Println("路由表未配置规则，添加 Internet 路由规则...")
		updateResp, err := networkClient.UpdateRouteTable(ctx, core.UpdateRouteTableRequest{
			RtId: routeTable.Id,
			UpdateRouteTableDetails: core.UpdateRouteTableDetails{
				RouteRules: []core.RouteRule{routeRule},
			},
			RequestMetadata: getRetryPolicy(),
		})
		if err != nil {
			return core.RouteTable{}, err
		}
		fmt.Println("路由规则添加成功")
		return updateResp.RouteTable, nil
	}

	return routeTable, nil
}

func createOrGetSubnet(vcnID *string) (core.Subnet, error) {
	resp, err := networkClient.ListSubnets(ctx, core.ListSubnetsRequest{
		CompartmentId:   common.String(oracle.Tenancy),
		VcnId:           vcnID,
		RequestMetadata: getRetryPolicy(),
	})
	if err != nil {
		return core.Subnet{}, err
	}

	displayName := instance.SubnetDisplayName
	if len(resp.Items) > 0 && displayName == "" {
		return resp.Items[0], nil
	}

	for _, subnet := range resp.Items {
		if *subnet.DisplayName == displayName {
			return subnet, nil
		}
	}

	// 创建子网
	fmt.Println("开始创建 Subnet...")
	if displayName == "" {
		displayName = time.Now().Format("subnet-20060102-1504")
	}

	createDetails := core.CreateSubnetDetails{
		CompartmentId: common.String(oracle.Tenancy),
		VcnId:         vcnID,
		CidrBlock:     common.String("10.0.0.0/20"),
		DisplayName:   common.String(displayName),
		DnsLabel:      common.String("subnetdns"),
	}

	// 如果需要 IPv6
	if instance.EnableIPv6 {
		vcnResp, err := networkClient.GetVcn(ctx, core.GetVcnRequest{
			VcnId:           vcnID,
			RequestMetadata: getRetryPolicy(),
		})
		if err == nil && len(vcnResp.Vcn.Ipv6CidrBlocks) > 0 {
			createDetails.Ipv6CidrBlocks = []string{}
		}
	}

	createResp, err := networkClient.CreateSubnet(ctx, core.CreateSubnetRequest{
		CreateSubnetDetails: createDetails,
		RequestMetadata:     getRetryPolicy(),
	})
	
	if err != nil {
		return core.Subnet{}, err
	}

	// 更新安全列表
	if err := updateSecurityList(createResp.Subnet.SecurityListIds); err != nil {
		fmt.Printf("警告: 更新安全列表失败: %v\n", err)
	}

	fmt.Printf("Subnet 创建成功: %s", *createResp.Subnet.DisplayName)
	if instance.EnableIPv6 && len(createResp.Subnet.Ipv6CidrBlocks) > 0 {
		fmt.Printf(" IPv6 CIDR: %s", strings.Join(createResp.Subnet.Ipv6CidrBlocks, ", "))
	}
	fmt.Println()
	
	return createResp.Subnet, nil
}

func updateSecurityList(securityListIDs []string) error {
	if len(securityListIDs) == 0 {
		return nil
	}

	getResp, err := networkClient.GetSecurityList(ctx, core.GetSecurityListRequest{
		SecurityListId:  common.String(securityListIDs[0]),
		RequestMetadata: getRetryPolicy(),
	})
	if err != nil {
		return err
	}

	// 构建入站规则
	ingressRules := getResp.IngressSecurityRules
	ingressRules = append(ingressRules,
		core.IngressSecurityRule{
			Protocol: common.String("all"),
			Source:   common.String("0.0.0.0/0"),
		},
	)

	// 如果需要 IPv6，添加 IPv6 规则
	if instance.EnableIPv6 {
		ingressRules = append(ingressRules,
			core.IngressSecurityRule{
				Protocol: common.String("all"),
				Source:   common.String("::/0"),
			},
		)
	}

	_, err = networkClient.UpdateSecurityList(ctx, core.UpdateSecurityListRequest{
		SecurityListId: common.String(securityListIDs[0]),
		UpdateSecurityListDetails: core.UpdateSecurityListDetails{
			IngressSecurityRules: ingressRules,
		},
		RequestMetadata: getRetryPolicy(),
	})

	return err
}

// ── 镜像 ──────────────────────────────────────────────────────────────────────

func getImage() (core.Image, error) {
	if instance.OperatingSystem == "" || instance.OperatingSystemVersion == "" {
		return core.Image{}, errors.New("操作系统类型和版本不能为空")
	}

	resp, err := computeClient.ListImages(ctx, core.ListImagesRequest{
		CompartmentId:          common.String(oracle.Tenancy),
		OperatingSystem:        common.String(instance.OperatingSystem),
		OperatingSystemVersion: common.String(instance.OperatingSystemVersion),
		Shape:                  common.String(instance.Shape),
		RequestMetadata:        getRetryPolicy(),
	})
	
	if err != nil {
		return core.Image{}, err
	}
	
	if len(resp.Items) == 0 {
		return core.Image{}, fmt.Errorf("未找到 [%s %s] 的镜像", 
			instance.OperatingSystem, instance.OperatingSystemVersion)
	}
	
	return resp.Items[0], nil
}

// ── 获取实例 IP ─────────────────────────────────────────────────────────────────

type InstanceIPs struct {
	IPv4 []string
	IPv6 []string
}

func getInstanceIPs(instanceID *string) (InstanceIPs, error) {
	result := InstanceIPs{}

	// 等待实例运行（最多 10 分钟）
	for i := 0; i < 60; i++ {
		resp, err := computeClient.GetInstance(ctx, core.GetInstanceRequest{
			InstanceId:      instanceID,
			RequestMetadata: getRetryPolicy(),
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

	// 获取 VNIC 附件
	vnicAttachments, err := computeClient.ListVnicAttachments(ctx, core.ListVnicAttachmentsRequest{
		CompartmentId:   common.String(oracle.Tenancy),
		InstanceId:      instanceID,
		RequestMetadata: getRetryPolicy(),
	})
	if err != nil {
		return result, err
	}

	for _, attachment := range vnicAttachments.Items {
		// 获取 VNIC 详情
		vnicResp, err := networkClient.GetVnic(ctx, core.GetVnicRequest{
			VnicId:          attachment.VnicId,
			RequestMetadata: getRetryPolicy(),
		})
		if err != nil {
			continue
		}
		
		if vnicResp.PublicIp != nil && *vnicResp.PublicIp != "" {
			result.IPv4 = append(result.IPv4, *vnicResp.PublicIp)
		}

		// 获取 IPv6 地址
		if instance.EnableIPv6 {
			ipv6Resp, err := networkClient.ListIpv6s(ctx, core.ListIpv6sRequest{
				VnicId:          attachment.VnicId,
				RequestMetadata: getRetryPolicy(),
			})
			if err == nil {
				for _, ipv6 := range ipv6Resp.Items {
					if ipv6.IpAddress != nil && *ipv6.IpAddress != "" {
						result.IPv6 = append(result.IPv6, *ipv6.IpAddress)
					}
				}
			}
		}
	}

	if len(result.IPv4) == 0 && len(result.IPv6) == 0 {
		return result, errors.New("未获取到任何公共 IP")
	}
	
	return result, nil
}

// ── 可用性域 ──────────────────────────────────────────────────────────────────────

func listAvailabilityDomains() ([]identity.AvailabilityDomain, error) {
	resp, err := identityClient.ListAvailabilityDomains(ctx, identity.ListAvailabilityDomainsRequest{
		CompartmentId:   common.String(oracle.Tenancy),
		RequestMetadata: getRetryPolicy(),
	})
	return resp.Items, err
}

// ── Telegram 消息 ─────────────────────────────────────────────────────────────────

func sendMessage(name, text string) {
	if token == "" || chatID == "" {
		return
	}

	data := url.Values{
		"parse_mode": {"Markdown"},
		"chat_id":    {chatID},
		"text":       {"🔰*甲骨文通知* " + name + "\n" + text},
	}

	req, err := http.NewRequest(http.MethodPost, sendMessageURL, strings.NewReader(data.Encode()))
	if err != nil {
		return
	}
	
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	
	client := getHTTPClient()
	_, err = client.Do(req)
	if err != nil {
		fmt.Printf("发送 Telegram 消息失败: %v\n", err)
	}
}

func getHTTPClient() *http.Client {
	if proxy == "" {
		return &http.Client{Timeout: 10 * time.Second}
	}
	
	proxyURL, err := url.Parse(proxy)
	if err != nil {
		return &http.Client{Timeout: 10 * time.Second}
	}
	
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		},
	}
}

// ── 工具函数 ──────────────────────────────────────────────────────────────────────

func setProxy(client *common.BaseClient) {
	if proxy == "" {
		return
	}
	
	proxyURL, err := url.Parse(proxy)
	if err != nil {
		return
	}
	
	client.HTTPClient = &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		},
	}
}

func getRetryPolicy() common.RequestMetadata {
	attempts := uint(3)
	policy := common.NewRetryPolicyWithOptions(
		common.WithMaximumNumberAttempts(attempts),
		common.WithShouldRetryOperation(func(r common.OCIOperationResponse) bool {
			return r.Error != nil
		}),
	)
	return common.RequestMetadata{RetryPolicy: &policy}
}

func sleepRandom(min, max int32) {
	var seconds int32
	if min <= 0 || max <= 0 {
		seconds = 1
	} else if min >= max {
		seconds = max
	} else {
		seconds = rand.Int31n(max-min) + min
	}
	
	fmt.Printf("%s Sleep %d 秒...\n", time.Now().Format("2006-01-02 15:04:05"), seconds)
	time.Sleep(time.Duration(seconds) * time.Second)
}

func formatDuration(d time.Duration) string {
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
		return "<1秒"
	}
	return strings.Join(parts, " ")
}

func logPrintf(accountName, format string, args ...interface{}) {
	timestamp := time.Now().Format("2006-01-02 15:04:05")
	msg := fmt.Sprintf(format, args...)
	fmt.Printf("%s \033[1;36m[%s]\033[0m %s\n", timestamp, accountName, msg)
}

func printError(desc string, err error) {
	if err != nil {
		fmt.Printf("\033[1;31mError: %s: %v\033[0m\n", desc, err)
	} else {
		fmt.Printf("\033[1;31mError: %s\033[0m\n", desc)
	}
}
