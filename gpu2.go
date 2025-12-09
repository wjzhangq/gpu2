// System Stats Collector - Optimized Version
// Supports Windows & Linux with timeout protection
// Fixed report URL, persistent ID, SMBIOS support

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/mem"
)

const (
	REPORT_URL      = "https://gpu.zhangwenjin.com/report"
	TIMEOUT         = 3 * time.Second
	COLLECT_TIMEOUT = 5 * time.Second
)

type OSInfo struct {
	Name         string `json:"name"`
	Version      string `json:"version"`
	Architecture string `json:"architecture"`
	SMBIOS       string `json:"smbios,omitempty"` // 硬件序列号
}

type CPUInfo struct {
	ID           int     `json:"id"`
	Model        string  `json:"model"`
	Cores        int     `json:"cores"`
	UsagePercent float64 `json:"usage_percent"`
}

type MemoryInfo struct {
	TotalGB      float64 `json:"total_gb"`
	UsedGB       float64 `json:"used_gb"`
	UsagePercent float64 `json:"usage_percent"`
}

type DiskInfo struct {
	Mount        string  `json:"mount"`
	TotalGB      float64 `json:"total_gb"`
	UsedGB       float64 `json:"used_gb"`
	UsagePercent float64 `json:"usage_percent"`
}

type GPUInfo struct {
	ID                 int     `json:"id"`
	Model              string  `json:"model"`
	UsagePercent       float64 `json:"usage_percent"`
	MemoryTotalGB      float64 `json:"memory_total_gb"`
	MemoryUsedGB       float64 `json:"memory_used_gb"`
	MemoryUsagePercent float64 `json:"memory_usage_percent"`
}

type SystemStats struct {
	ID       string     `json:"id"`
	Hostname string     `json:"hostname"`
	OS       OSInfo     `json:"os"`
	CPUs     []CPUInfo  `json:"cpus"`
	Memory   MemoryInfo `json:"memory"`
	Disks    []DiskInfo `json:"disks"`
	GPUs     []GPUInfo  `json:"gpus"`
	TS       int64      `json:"timestamp"`
}

var (
	sysInfo       SystemStats
	nvidiaSmiPath string
	mutex         sync.RWMutex
)

// 获取或生成 ID (仅内存)
func getOrCreateID(customID string) string {
	if customID != "" {
		return customID
	}

	// 生成新 ID (不保存到文件)
	return uuid.New().String()
}

// 获取 SMBIOS 信息 (硬件序列号)
func getSMBIOS() string {
	var cmd *exec.Cmd

	if runtime.GOOS == "windows" {
		// Windows: 使用 WMIC 获取主板序列号
		cmd = exec.Command("wmic", "bios", "get", "serialnumber")
	} else {
		// Linux: 尝试读取 DMI 信息
		if data, err := os.ReadFile("/sys/class/dmi/id/product_uuid"); err == nil {
			return strings.TrimSpace(string(data))
		}
		// 备选: dmidecode (需要 root)
		cmd = exec.Command("dmidecode", "-s", "system-uuid")
	}

	if cmd != nil {
		_, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		if out, err := cmd.Output(); err == nil {
			lines := strings.Split(string(out), "\n")
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if line != "" && !strings.Contains(strings.ToLower(line), "serial") {
					return line
				}
			}
		}
	}

	return ""
}

func findNvidiaSmi() string {
	if p, err := exec.LookPath("nvidia-smi"); err == nil {
		return p
	}
	if runtime.GOOS == "windows" {
		base := `C:\Windows\System32\DriverStore\FileRepository\`
		matches, _ := filepath.Glob(base + "nvdm*")
		for _, m := range matches {
			exe := filepath.Join(m, "nvidia-smi.exe")
			if _, err := os.Stat(exe); err == nil {
				return exe
			}
		}
	}
	return ""
}

func initStaticInfo() {
	sysInfo.Hostname, _ = os.Hostname()

	// OS 信息
	hi, _ := host.Info()
	sysInfo.OS = OSInfo{
		Name:         hi.Platform + " " + hi.PlatformVersion,
		Version:      hi.KernelVersion,
		Architecture: runtime.GOARCH,
		SMBIOS:       getSMBIOS(),
	}

	// CPU 信息 - 带超时保护
	done := make(chan bool, 1)
	go func() {
		cpuInfo, _ := cpu.Info()
		for i, c := range cpuInfo {
			sysInfo.CPUs = append(sysInfo.CPUs, CPUInfo{
				ID:    i,
				Model: c.ModelName,
				Cores: int(c.Cores),
			})
		}
		done <- true
	}()

	select {
	case <-done:
	case <-time.After(COLLECT_TIMEOUT):
		fmt.Fprintln(os.Stderr, "Warning: CPU info collection timeout")
	}

	// Memory 总量
	if vm, err := mem.VirtualMemory(); err == nil {
		sysInfo.Memory.TotalGB = float64(vm.Total) / 1e9
	}

	// Disks - 带超时保护
	go func() {
		parts, _ := disk.Partitions(true)
		var disks []DiskInfo
		excludedMounts := []string{"/sys", "/proc", "/dev", "/run", "/snap", "/System"}
		for _, p := range parts {
			d, err := disk.Usage(p.Mountpoint)
			if err == nil && d.Total >= 1e9 { // 排除 total_gb < 1
				exclude := false
				for _, ex := range excludedMounts {
					if strings.HasPrefix(p.Mountpoint, ex) {
						exclude = true
						break
					}
				}
				if !exclude {
					disks = append(disks, DiskInfo{
						Mount:   p.Mountpoint,
						TotalGB: float64(d.Total) / 1e9,
					})
				}
			}
		}
		mutex.Lock()
		sysInfo.Disks = disks
		mutex.Unlock()
		done <- true
	}()

	select {
	case <-done:
	case <-time.After(COLLECT_TIMEOUT):
		fmt.Fprintln(os.Stderr, "Warning: Disk info collection timeout")
	}

	// GPU 静态信息
	nvidiaSmiPath = findNvidiaSmi()
	if nvidiaSmiPath != "" {
		updateGPU(true)
	}
}

// 并发更新动态信息
func updateDynamicInfo() {
	var wg sync.WaitGroup
	wg.Add(4)

	// CPU 使用率
	go func() {
		defer wg.Done()
		done := make(chan []float64, 1)
		go func() {
			p, _ := cpu.Percent(500*time.Millisecond, true)
			done <- p
		}()

		select {
		case p := <-done:
			mutex.Lock()
			for i := range sysInfo.CPUs {
				if i < len(p) {
					sysInfo.CPUs[i].UsagePercent = p[i]
				}
			}
			mutex.Unlock()
		case <-time.After(COLLECT_TIMEOUT):
			fmt.Fprintln(os.Stderr, "Warning: CPU usage timeout")
		}
	}()

	// 内存使用
	go func() {
		defer wg.Done()
		if vm, err := mem.VirtualMemory(); err == nil {
			mutex.Lock()
			sysInfo.Memory.UsedGB = float64(vm.Used) / 1e9
			sysInfo.Memory.UsagePercent = vm.UsedPercent
			mutex.Unlock()
		}
	}()

	// 磁盘使用
	go func() {
		defer wg.Done()
		mutex.RLock()
		disks := make([]DiskInfo, len(sysInfo.Disks))
		copy(disks, sysInfo.Disks)
		mutex.RUnlock()

		for i := range disks {
			if d, err := disk.Usage(disks[i].Mount); err == nil {
				disks[i].UsedGB = float64(d.Used) / 1e9
				disks[i].UsagePercent = d.UsedPercent
			}
		}

		mutex.Lock()
		sysInfo.Disks = disks
		mutex.Unlock()
	}()

	// GPU 使用
	go func() {
		defer wg.Done()
		updateGPU(false)
	}()

	wg.Wait()
}

func updateGPU(isInit bool) {
	if nvidiaSmiPath == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), COLLECT_TIMEOUT)
	defer cancel()

	cmd := exec.CommandContext(ctx, nvidiaSmiPath,
		"--query-gpu=name,utilization.gpu,memory.total,memory.used",
		"--format=csv,noheader,nounits")

	out, err := cmd.Output()
	if err != nil {
		return
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")

	mutex.Lock()
	defer mutex.Unlock()

	if isInit {
		sysInfo.GPUs = []GPUInfo{}
	}

	for i, line := range lines {
		fields := strings.Split(line, ",")
		if len(fields) < 4 {
			continue
		}

		model := strings.TrimSpace(fields[0])
		usage := parseFloat(fields[1])
		memTotal := parseFloat(fields[2]) / 1024.0
		memUsed := parseFloat(fields[3]) / 1024.0
		memPercent := 0.0
		if memTotal > 0 {
			memPercent = (memUsed / memTotal) * 100
		}

		if isInit {
			sysInfo.GPUs = append(sysInfo.GPUs, GPUInfo{
				ID:            i,
				Model:         model,
				MemoryTotalGB: memTotal,
			})
		}

		if i < len(sysInfo.GPUs) {
			sysInfo.GPUs[i].UsagePercent = usage
			sysInfo.GPUs[i].MemoryUsedGB = memUsed
			sysInfo.GPUs[i].MemoryUsagePercent = memPercent
		}
	}
}

func parseFloat(s string) float64 {
	v, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return v
}

func report(REPORT_URL string) {
	mutex.RLock()
	data, err := json.Marshal(sysInfo)
	mutex.RUnlock()

	if err != nil {
		return
	}

	req, err := http.NewRequest("POST", REPORT_URL, bytes.NewBuffer(data))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")

	ctx, cancel := context.WithTimeout(context.Background(), TIMEOUT)
	defer cancel()
	req = req.WithContext(ctx)

	client := &http.Client{Timeout: TIMEOUT}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Report failed: %v\n", err)
		return
	}
	defer resp.Body.Close()

	_, _ = io.ReadAll(resp.Body) // 消费响应体
}

func main() {
	interval := flag.Duration("interval", 5*time.Second, "collect interval")
	customID := flag.String("id", "", "custom system ID (auto-generated if not provided)")
	reportURL := flag.String("url", "https://gpu.zhangwenjin.com/report", "report URL")
	flag.Parse()

	// 初始化 ID
	sysInfo.ID = getOrCreateID(*customID)

	// 初始化静态信息
	fmt.Println("Initializing system info...")
	initStaticInfo()

	// 首次更新动态信息
	updateDynamicInfo()
	sysInfo.TS = time.Now().Unix()

	// 打印首次信息
	mutex.RLock()
	data, _ := json.MarshalIndent(sysInfo, "", "  ")
	mutex.RUnlock()
	fmt.Println(string(data))

	// 首次上报
	go report(*reportURL)

	// 定期采集和上报
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()

	for range ticker.C {
		updateDynamicInfo()

		mutex.Lock()
		sysInfo.TS = time.Now().Unix()
		mutex.Unlock()

		// 打印更新后的信息
		mutex.RLock()
		data, _ := json.MarshalIndent(sysInfo, "", "  ")
		mutex.RUnlock()
		fmt.Println(string(data))

		// 异步上报
		go report(*reportURL)
	}
}
