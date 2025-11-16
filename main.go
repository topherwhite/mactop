// Copyright (c) 2024-2025 Carsen Klock under MIT License
// mactop is a simple terminal based Apple Silicon power monitor written in Go Lang! github.com/context-labs/mactop
package main

/*
#cgo LDFLAGS: -framework CoreFoundation -framework IOKit
#include <mach/mach_host.h>
#include <mach/processor_info.h>
#include <mach/mach_init.h>

extern kern_return_t vm_deallocate(vm_map_t target_task, vm_address_t address, vm_size_t size);
*/
import "C"
import (
	"bufio"
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/shirou/gopsutil/mem"
	"howett.net/plist"
)

var (
	version        = "v0.2.3"
	stderrLogger   = log.New(os.Stderr, "", 0)
	updateInterval = 1000
	done           = make(chan struct{})
	lastCPUTimes   []CPUUsage
	firstRun       = true
	prometheusPort string
)

var (
	// Prometheus metrics
	cpuUsage = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "mactop_cpu_usage_percent",
			Help: "Current Total CPU usage percentage",
		},
	)

	eCoreUsage = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "mactop_ecore_usage_percent",
			Help: "Current average efficiency core (E-core) usage percentage",
		},
	)

	pCoreUsage = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "mactop_pcore_usage_percent",
			Help: "Current average performance core (P-core) usage percentage",
		},
	)

	gpuUsage = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "mactop_gpu_usage_percent",
			Help: "Current GPU usage percentage",
		},
	)

	gpuFreqMHz = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "mactop_gpu_freq_mhz",
			Help: "Current GPU frequency in MHz",
		},
	)

	memoryUsage = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "mactop_memory_gb",
			Help: "Memory usage in GB",
		},
		[]string{"type"}, // "used", "total", "swap_used", "swap_total"
	)

	networkActivity = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "mactop_network_activity_mb",
			Help: "Network activity in MB/s",
		},
		[]string{"type"}, // "in", "out"
	)

	diskActivity = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "mactop_disk_activity_mb",
			Help: "Disk activity in MB/s",
		},
		[]string{"type"}, // "read", "write"
	)

	sysStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "mactop_system_status",
			Help: "System Status",
		},
		[]string{"type"}, // "used", "total", "swap_used", "swap_total"
	)
)

func startPrometheusServer(port string) {
	registry := prometheus.NewRegistry()
	registry.MustRegister(cpuUsage)
	registry.MustRegister(eCoreUsage)
	registry.MustRegister(pCoreUsage)
	registry.MustRegister(gpuUsage)
	registry.MustRegister(gpuFreqMHz)
	registry.MustRegister(memoryUsage)
	registry.MustRegister(networkActivity)
	registry.MustRegister(diskActivity)
	registry.MustRegister(sysStatus)

	handler := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})

	http.Handle("/metrics", handler)
	go func() {
		err := http.ListenAndServe(":"+port, nil)
		if err != nil {
			stderrLogger.Printf("Failed to start Prometheus metrics server: %v\n", err)
		}
	}()
}

type CPUUsage struct {
	User   float64
	System float64
	Idle   float64
	Nice   float64
}

type CPUMetrics struct {
	EClusterActive, EClusterFreqMHz, PClusterActive, PClusterFreqMHz int
	ECores, PCores                                                   []int
	CoreMetrics                                                      map[string]int
	CPUW, GPUW, PackageW                                             float64
	CoreUsages                                                       []float64
	Throttled                                                        bool
}

type NetDiskMetrics struct {
	OutPacketsPerSec, OutBytesPerSec, InPacketsPerSec, InBytesPerSec, ReadOpsPerSec, WriteOpsPerSec, ReadKBytesPerSec, WriteKBytesPerSec float64
}

type GPUMetrics struct {
	FreqMHz, Active int
}

type MemoryMetrics struct {
	Total, Used, Available, SwapTotal, SwapUsed uint64
}

// CoreTopology describes how cores are arranged in the CPU usage array
type CoreTopology struct {
	// PCoreIndices contains the indices of all P-cores in the CPU usage array
	PCoreIndices []int
	// ECoreIndices contains the indices of all E-cores in the CPU usage array
	ECoreIndices []int
	// Description provides human-readable info about the topology
	Description string
}

// ChipVariant uniquely identifies a chip configuration
type ChipVariant struct {
	ModelName  string // e.g., "M3 Ultra", "M4 Pro"
	PCoreCount int
	ECoreCount int
}

// getChipVariant returns a unique identifier for the current chip
func getChipVariant(cpuName string, pCoreCount, eCoreCount int) ChipVariant {
	// Extract model name (e.g., "Apple M3 Ultra" -> "M3 Ultra")
	modelName := cpuName
	if strings.HasPrefix(cpuName, "Apple ") {
		modelName = strings.TrimPrefix(cpuName, "Apple ")
	}

	return ChipVariant{
		ModelName:  modelName,
		PCoreCount: pCoreCount,
		ECoreCount: eCoreCount,
	}
}

// getCoreTopology returns the core topology for a given chip variant
func getCoreTopology(variant ChipVariant) CoreTopology {
	totalCores := variant.PCoreCount + variant.ECoreCount

	// Check for known chip-specific topologies
	switch {
	case strings.Contains(variant.ModelName, "M3 Ultra") && variant.PCoreCount == 24 && variant.ECoreCount == 8:
		// M3 Ultra 32-core (24P + 8E): E-cores first within each die
		// Die 1: E-cores 0-3, P-cores 4-15
		// Die 2: E-cores 16-19, P-cores 20-31
		topology := CoreTopology{
			Description:  "M3 Ultra 32-core: E-cores first within each die",
			PCoreIndices: make([]int, 0, 24),
			ECoreIndices: make([]int, 0, 8),
		}
		// Die 1 E-cores: 0-3
		for i := 0; i < 4; i++ {
			topology.ECoreIndices = append(topology.ECoreIndices, i)
		}
		// Die 1 P-cores: 4-15
		for i := 4; i < 16; i++ {
			topology.PCoreIndices = append(topology.PCoreIndices, i)
		}
		// Die 2 E-cores: 16-19
		for i := 16; i < 20; i++ {
			topology.ECoreIndices = append(topology.ECoreIndices, i)
		}
		// Die 2 P-cores: 20-31
		for i := 20; i < 32; i++ {
			topology.PCoreIndices = append(topology.PCoreIndices, i)
		}
		return topology

	case strings.Contains(variant.ModelName, "M3 Ultra") && variant.PCoreCount == 20 && variant.ECoreCount == 8:
		// M3 Ultra 28-core (20P + 8E): Need to determine correct mapping
		// Based on M3 Ultra architecture, likely same pattern but with fewer P-cores per die
		// Die 1: E-cores 0-3, P-cores 4-13
		// Die 2: E-cores 14-17, P-cores 18-27
		topology := CoreTopology{
			Description:  "M3 Ultra 28-core: E-cores first within each die",
			PCoreIndices: make([]int, 0, 20),
			ECoreIndices: make([]int, 0, 8),
		}
		// Die 1 E-cores: 0-3
		for i := 0; i < 4; i++ {
			topology.ECoreIndices = append(topology.ECoreIndices, i)
		}
		// Die 1 P-cores: 4-13
		for i := 4; i < 14; i++ {
			topology.PCoreIndices = append(topology.PCoreIndices, i)
		}
		// Die 2 E-cores: 14-17
		for i := 14; i < 18; i++ {
			topology.ECoreIndices = append(topology.ECoreIndices, i)
		}
		// Die 2 P-cores: 18-27
		for i := 18; i < 28; i++ {
			topology.PCoreIndices = append(topology.PCoreIndices, i)
		}
		return topology

	case strings.Contains(variant.ModelName, "M4 Pro"):
		// M4 Pro: P-cores first, then E-cores (standard non-Ultra layout)
		topology := CoreTopology{
			Description:  "M4 Pro: P-cores first, then E-cores",
			PCoreIndices: make([]int, 0, variant.PCoreCount),
			ECoreIndices: make([]int, 0, variant.ECoreCount),
		}
		for i := 0; i < variant.PCoreCount; i++ {
			topology.PCoreIndices = append(topology.PCoreIndices, i)
		}
		for i := variant.PCoreCount; i < totalCores; i++ {
			topology.ECoreIndices = append(topology.ECoreIndices, i)
		}
		return topology

	case strings.Contains(variant.ModelName, "M1 Ultra") || strings.Contains(variant.ModelName, "M2 Ultra"):
		// M1/M2 Ultra: P-cores first within each die (interleaved pattern)
		// Die 1: P-cores, E-cores
		// Die 2: P-cores, E-cores
		pCoresPerDie := variant.PCoreCount / 2
		eCoresPerDie := variant.ECoreCount / 2
		topology := CoreTopology{
			Description:  "M1/M2 Ultra: P-cores first within each die",
			PCoreIndices: make([]int, 0, variant.PCoreCount),
			ECoreIndices: make([]int, 0, variant.ECoreCount),
		}
		// Die 1 P-cores
		for i := 0; i < pCoresPerDie; i++ {
			topology.PCoreIndices = append(topology.PCoreIndices, i)
		}
		// Die 1 E-cores
		for i := pCoresPerDie; i < pCoresPerDie+eCoresPerDie; i++ {
			topology.ECoreIndices = append(topology.ECoreIndices, i)
		}
		// Die 2 P-cores
		for i := pCoresPerDie + eCoresPerDie; i < 2*pCoresPerDie+eCoresPerDie; i++ {
			topology.PCoreIndices = append(topology.PCoreIndices, i)
		}
		// Die 2 E-cores
		for i := 2*pCoresPerDie + eCoresPerDie; i < totalCores; i++ {
			topology.ECoreIndices = append(topology.ECoreIndices, i)
		}
		return topology

	default:
		// Default for most chips: P-cores first, then E-cores
		topology := CoreTopology{
			Description:  "Standard layout: P-cores first, then E-cores",
			PCoreIndices: make([]int, 0, variant.PCoreCount),
			ECoreIndices: make([]int, 0, variant.ECoreCount),
		}
		for i := 0; i < variant.PCoreCount; i++ {
			topology.PCoreIndices = append(topology.PCoreIndices, i)
		}
		for i := variant.PCoreCount; i < totalCores; i++ {
			topology.ECoreIndices = append(topology.ECoreIndices, i)
		}
		return topology
	}
}


func NewCPUMetrics() CPUMetrics {
	return CPUMetrics{
		CoreMetrics: make(map[string]int),
		ECores:      make([]int, 0),
		PCores:      make([]int, 0),
	}
}


func GetCPUPercentages() ([]float64, error) {
	currentTimes, err := GetCPUUsage()
	if err != nil {
		return nil, err
	}
	if firstRun {
		lastCPUTimes = currentTimes
		firstRun = false
		return make([]float64, len(currentTimes)), nil
	}
	percentages := make([]float64, len(currentTimes))
	for i := range currentTimes {
		totalDelta := (currentTimes[i].User - lastCPUTimes[i].User) +
			(currentTimes[i].System - lastCPUTimes[i].System) +
			(currentTimes[i].Idle - lastCPUTimes[i].Idle) +
			(currentTimes[i].Nice - lastCPUTimes[i].Nice)

		activeDelta := (currentTimes[i].User - lastCPUTimes[i].User) +
			(currentTimes[i].System - lastCPUTimes[i].System) +
			(currentTimes[i].Nice - lastCPUTimes[i].Nice)

		if totalDelta > 0 {
			percentages[i] = (activeDelta / totalDelta) * 100.0
		}
		if percentages[i] < 0 {
			percentages[i] = 0
		} else if percentages[i] > 100 {
			percentages[i] = 100
		}
	}
	lastCPUTimes = currentTimes
	return percentages, nil
}

func GetCPUUsage() ([]CPUUsage, error) {
	var numCPUs C.natural_t
	var cpuLoad *C.processor_cpu_load_info_data_t
	var cpuMsgCount C.mach_msg_type_number_t
	host := C.mach_host_self()
	kernReturn := C.host_processor_info(
		host,
		C.PROCESSOR_CPU_LOAD_INFO,
		&numCPUs,
		(*C.processor_info_array_t)(unsafe.Pointer(&cpuLoad)),
		&cpuMsgCount,
	)
	if kernReturn != C.KERN_SUCCESS {
		return nil, fmt.Errorf("error getting CPU info: %d", kernReturn)
	}
	defer C.vm_deallocate(
		C.mach_task_self_,
		(C.vm_address_t)(uintptr(unsafe.Pointer(cpuLoad))),
		C.vm_size_t(cpuMsgCount)*C.sizeof_processor_cpu_load_info_data_t,
	)
	cpuLoadInfo := (*[1 << 30]C.processor_cpu_load_info_data_t)(unsafe.Pointer(cpuLoad))[:numCPUs:numCPUs]
	cpuUsage := make([]CPUUsage, numCPUs)
	for i := 0; i < int(numCPUs); i++ {
		cpuUsage[i] = CPUUsage{
			User:   float64(cpuLoadInfo[i].cpu_ticks[C.CPU_STATE_USER]),
			System: float64(cpuLoadInfo[i].cpu_ticks[C.CPU_STATE_SYSTEM]),
			Idle:   float64(cpuLoadInfo[i].cpu_ticks[C.CPU_STATE_IDLE]),
			Nice:   float64(cpuLoadInfo[i].cpu_ticks[C.CPU_STATE_NICE]),
		}
	}
	return cpuUsage, nil
}



func StderrToLogfile(logfile *os.File) {
	syscall.Dup2(int(logfile.Fd()), 2)
}


func main() {
	var (
		interval              int
		err                   error
		setInterval bool
	)
	for i := 1; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--help", "-h":
			fmt.Print("Usage: mactop [--help] [--version] [--interval] [--prometheus]\n--help: Show this help message\n--version: Show the version of mactop\n--interval: Set the powermetrics update interval in milliseconds. Default is 1000.\n--prometheus, -p: Set the Prometheus metrics port. Required. (e.g. --prometheus=9090)\n\nYou must use sudo to run mactop, as powermetrics requires root privileges.\n\nFor more information, see https://github.com/context-labs/mactop written by Carsen Klock.\n")
			os.Exit(0)
		case "--version", "-v":
			fmt.Println("mactop version:", version)
			os.Exit(0)
		case "--prometheus", "-p":
			if i+1 < len(os.Args) {
				prometheusPort = os.Args[i+1]
				i++
			} else {
				fmt.Println("Error: --prometheus flag requires a port number")
				os.Exit(1)
			}
		case "--interval", "-i":
			if i+1 < len(os.Args) {
				interval, err = strconv.Atoi(os.Args[i+1])
				if err != nil {
					fmt.Println("Invalid interval:", err)
					os.Exit(1)
				}
				setInterval = true
				i++
			} else {
				fmt.Println("Error: --interval flag requires an interval value")
				os.Exit(1)
			}
		}
	}
	
	if prometheusPort == "" {
		fmt.Println("Error: Prometheus port is required. Use --prometheus=<port>")
		os.Exit(1)
	}
	
	if os.Geteuid() != 0 {
		fmt.Println("Welcome to mactop! Please try again and run mactop with sudo privileges!")
		fmt.Println("Usage: sudo mactop --prometheus=<port>")
		os.Exit(1)
	}
	
	logfile, err := setupLogfile()
	if err != nil {
		stderrLogger.Fatalf("failed to setup log file: %v", err)
	}
	defer logfile.Close()
	StderrToLogfile(logfile)

	// Log the system information
	appleSiliconModel := getSOCInfo()
	modelName, _ := appleSiliconModel["name"].(string)
	eCoreCount, _ := appleSiliconModel["e_core_count"].(int)
	pCoreCount, _ := appleSiliconModel["p_core_count"].(int)
	gpuCoreCount, _ := appleSiliconModel["gpu_core_count"].(string)
	
	stderrLogger.Printf("Starting mactop in Prometheus-only mode")
	stderrLogger.Printf("Model: %s\nE-Core Count: %d\nP-Core Count: %d\nGPU Core Count: %s", 
		modelName, eCoreCount, pCoreCount, gpuCoreCount)
	
	startPrometheusServer(prometheusPort)
	fmt.Printf("Prometheus metrics server started on port %s\n", prometheusPort)
	fmt.Printf("Metrics available at http://localhost:%s/metrics\n", prometheusPort)
	
	if setInterval {
		updateInterval = interval
	}
	
	cpuMetricsChan := make(chan CPUMetrics, 1)
	gpuMetricsChan := make(chan GPUMetrics, 1)
	netdiskMetricsChan := make(chan NetDiskMetrics, 1)
	
	go collectMetrics(done, cpuMetricsChan, gpuMetricsChan, netdiskMetricsChan)
	
	go func() {
		ticker := time.NewTicker(time.Duration(updateInterval) * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case cpuMetrics := <-cpuMetricsChan:
				updateCPUPrometheus(cpuMetrics)
			case gpuMetrics := <-gpuMetricsChan:
				updateGPUPrometheus(gpuMetrics)
			case netdiskMetrics := <-netdiskMetricsChan:
				updateNetDiskPrometheus(netdiskMetrics)
			case <-ticker.C:
				percentages, err := GetCPUPercentages()
				if err != nil {
					stderrLogger.Printf("Error getting CPU percentages: %v\n", err)
					continue
				}
				var totalUsage float64
				for _, usage := range percentages {
					totalUsage += usage
				}
				totalUsage /= float64(len(percentages))
				cpuUsage.Set(totalUsage)
			case <-done:
				return
			}
		}
	}()
	
	// Wait for interrupt signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	
	fmt.Println("Shutting down...")
	close(done)
}

func setupLogfile() (*os.File, error) {
	if err := os.MkdirAll("/var/log", 0755); err != nil {
		return nil, fmt.Errorf("failed to make the log directory: %v", err)
	}
	logfile, err := os.OpenFile("/var/log/mactop.log", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0660)
	if err != nil {
		return nil, fmt.Errorf("failed to open log file: %v", err)
	}
	log.SetFlags(log.Ltime | log.Lshortfile)
	log.SetOutput(logfile)
	return logfile, nil
}

func collectMetrics(done chan struct{}, cpumetricsChan chan CPUMetrics, gpumetricsChan chan GPUMetrics, netdiskMetricsChan chan NetDiskMetrics) {
	cpumetricsChan <- CPUMetrics{}
	gpumetricsChan <- GPUMetrics{}
	netdiskMetricsChan <- NetDiskMetrics{}
	cmd := exec.Command("sudo", "powermetrics", "--samplers", "cpu_power,gpu_power,thermal,network,disk", "--show-initial-usage", "-f", "plist", "-i", strconv.Itoa(updateInterval))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stderrLogger.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		stderrLogger.Fatal(err)
	}

	defer func() {
		if err := cmd.Process.Kill(); err != nil {
			stderrLogger.Fatalf("ERROR: Failed to kill powermetrics: %v", err)
		}
	}()

	// Create buffered reader with larger buffer
	const bufferSize = 10 * 1024 * 1024 // 10MB
	reader := bufio.NewReaderSize(stdout, bufferSize)

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, bufferSize), bufferSize)

	scanner.Split(func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		if atEOF && len(data) == 0 {
			return 0, nil, nil
		}
		start := bytes.Index(data, []byte("<?xml"))
		if start == -1 {
			start = bytes.Index(data, []byte("<plist"))
		}
		if start >= 0 {
			if end := bytes.Index(data[start:], []byte("</plist>")); end >= 0 {
				return start + end + 8, data[start : start+end+8], nil
			}
		}
		if atEOF {
			if start >= 0 {
				return len(data), data[start:], nil
			}
			return len(data), nil, nil
		}
		return 0, nil, nil
	})
	retryCount := 0
	maxRetries := 3
	for scanner.Scan() {
		select {
		case <-done:
			return
		default:
			plistData := scanner.Text()
			if !strings.Contains(plistData, "<?xml") || !strings.Contains(plistData, "</plist>") {
				retryCount++
				if retryCount >= maxRetries {
					retryCount = 0
					continue
				}
				continue
			}
			retryCount = 0 // Reset retry counter on successful parse
			var data map[string]interface{}
			err := plist.NewDecoder(strings.NewReader(plistData)).Decode(&data)
			if err != nil {
				stderrLogger.Printf("Error decoding plist: %v", err)
				continue
			}
			cpuMetrics := parseCPUMetrics(data, NewCPUMetrics())
			gpuMetrics := parseGPUMetrics(data)
			netdiskMetrics := parseNetDiskMetrics(data)

			// Non-blocking sends
			select {
			case cpumetricsChan <- cpuMetrics:
			default:
			}
			select {
			case gpumetricsChan <- gpuMetrics:
			default:
			}
			select {
			case netdiskMetricsChan <- netdiskMetrics:
			default:
			}
		}
	}
}

func parseGPUMetrics(data map[string]interface{}) GPUMetrics {
	var gpuMetrics GPUMetrics
	if gpu, ok := data["gpu"].(map[string]interface{}); ok {
		if freqHz, ok := gpu["freq_hz"].(float64); ok {
			gpuMetrics.FreqMHz = int(freqHz)
		}
		if idleRatio, ok := gpu["idle_ratio"].(float64); ok {
			gpuMetrics.Active = int((1 - idleRatio) * 100)
		}
	}
	return gpuMetrics
}

func parseNetDiskMetrics(data map[string]interface{}) NetDiskMetrics {
	var metrics NetDiskMetrics
	if network, ok := data["network"].(map[string]interface{}); ok {
		if rate, ok := network["ibyte_rate"].(float64); ok {
			metrics.InBytesPerSec = rate / 1000
		}
		if rate, ok := network["obyte_rate"].(float64); ok {
			metrics.OutBytesPerSec = rate / 1000
		}
		if rate, ok := network["ipacket_rate"].(float64); ok {
			metrics.InPacketsPerSec = rate
		}
		if rate, ok := network["opacket_rate"].(float64); ok {
			metrics.OutPacketsPerSec = rate
		}
	}
	if disk, ok := data["disk"].(map[string]interface{}); ok {
		if rate, ok := disk["rbytes_per_s"].(float64); ok {
			metrics.ReadKBytesPerSec = rate / 1000
		}
		if rate, ok := disk["wbytes_per_s"].(float64); ok {
			metrics.WriteKBytesPerSec = rate / 1000
		}
		if rate, ok := disk["rops_per_s"].(float64); ok {
			metrics.ReadOpsPerSec = rate
		}
		if rate, ok := disk["wops_per_s"].(float64); ok {
			metrics.WriteOpsPerSec = rate
		}
	}
	return metrics
}




func updateCPUPrometheus(cpuMetrics CPUMetrics) {
	coreUsages, err := GetCPUPercentages()
	if err != nil {
		stderrLogger.Printf("Error getting CPU percentages: %v\n", err)
		return
	}

	// Get SOC info for core counts
	appleSiliconModel := getSOCInfo()
	eCoreCount, _ := appleSiliconModel["e_core_count"].(int)
	pCoreCount, _ := appleSiliconModel["p_core_count"].(int)
	cpuName, _ := appleSiliconModel["name"].(string)

	// Get chip variant and topology
	variant := getChipVariant(cpuName, pCoreCount, eCoreCount)
	topology := getCoreTopology(variant)

	stderrLogger.Printf("Chip: %s (%dP + %dE cores), Total cores: %d",
		variant.ModelName, pCoreCount, eCoreCount, len(coreUsages))
	stderrLogger.Printf("Topology: %s", topology.Description)

	// Calculate average for all cores
	var totalUsage float64
	for _, usage := range coreUsages {
		totalUsage += usage
	}
	totalUsage /= float64(len(coreUsages))

	// Calculate P-core average using topology
	var pCoreTotal float64
	if len(topology.PCoreIndices) > 0 {
		stderrLogger.Printf("P-cores usage values:")
		for _, idx := range topology.PCoreIndices {
			if idx < len(coreUsages) {
				stderrLogger.Printf("  P-core at index %d: %.2f%%", idx, coreUsages[idx])
				pCoreTotal += coreUsages[idx]
			}
		}
		pCoreTotal /= float64(len(topology.PCoreIndices))
	}

	// Calculate E-core average using topology
	var eCoreTotal float64
	if len(topology.ECoreIndices) > 0 {
		stderrLogger.Printf("E-cores usage values:")
		for _, idx := range topology.ECoreIndices {
			if idx < len(coreUsages) {
				stderrLogger.Printf("  E-core at index %d: %.2f%%", idx, coreUsages[idx])
				eCoreTotal += coreUsages[idx]
			}
		}
		eCoreTotal /= float64(len(topology.ECoreIndices))
	}

	memoryMetrics := getMemoryMetrics()

	// Set all CPU related metrics in Prometheus
	cpuUsage.Set(float64(totalUsage))
	eCoreUsage.Set(float64(eCoreTotal))
	pCoreUsage.Set(float64(pCoreTotal))
	
	// Log the CPU usage values
	stderrLogger.Printf("CPU Usage - Total: %.2f%%, E-cores: %.2f%%, P-cores: %.2f%%", 
		totalUsage, eCoreTotal, pCoreTotal)

	memoryUsage.With(prometheus.Labels{"type": "used"}).Set(float64(memoryMetrics.Used) / 1024 / 1024 / 1024)
	memoryUsage.With(prometheus.Labels{"type": "total"}).Set(float64(memoryMetrics.Total) / 1024 / 1024 / 1024)
	memoryUsage.With(prometheus.Labels{"type": "swap_used"}).Set(float64(memoryMetrics.SwapUsed) / 1024 / 1024 / 1024)
	memoryUsage.With(prometheus.Labels{"type": "swap_total"}).Set(float64(memoryMetrics.SwapTotal) / 1024 / 1024 / 1024)

	sysStatus.With(prometheus.Labels{"type": "p_cores"}).Set(float64(pCoreCount))
	sysStatus.With(prometheus.Labels{"type": "e_cores"}).Set(float64(eCoreCount))
	sysStatus.With(prometheus.Labels{"type": "is_throttled"}).Set(map[bool]float64{true:1,false:0}[cpuMetrics.Throttled])
}


func updateGPUPrometheus(gpuMetrics GPUMetrics) {

	gpuUsage.Set(float64(gpuMetrics.Active))
	gpuFreqMHz.Set(float64(gpuMetrics.FreqMHz))
	gpus_cnt, _ := strconv.ParseUint(getGPUCores(), 10, 32)
	sysStatus.With(prometheus.Labels{"type": "g_cores"}).Set(float64(gpus_cnt))
}

func getDiskStorage() (total, used, available string) {
	cmd := exec.Command("df", "-h", "/")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	output, err := cmd.Output()
	if err != nil {
		return "N/A", "N/A", "N/A"
	}
	lines := strings.Split(string(output), "\n")
	if len(lines) < 2 {
		return "N/A", "N/A", "N/A"
	}
	fields := strings.Fields(lines[1])
	if len(fields) < 6 {
		return "N/A", "N/A", "N/A"
	}
	totalBytes := parseSize(fields[1])
	availBytes := parseSize(fields[3])
	usedBytes := totalBytes - availBytes
	return formatGigabytes(totalBytes), formatGigabytes(usedBytes), formatGigabytes(availBytes)
}

func parseSize(size string) float64 {
	var value float64
	var unit string
	fmt.Sscanf(size, "%f%s", &value, &unit)
	multiplier := 1.0
	switch strings.ToLower(strings.TrimSuffix(unit, "i")) {
	case "k", "kb":
		multiplier = 1000
	case "m", "mb":
		multiplier = 1000 * 1000
	case "g", "gb":
		multiplier = 1000 * 1000 * 1000
	case "t", "tb":
		multiplier = 1000 * 1000 * 1000 * 1000
	}
	return value * multiplier
}

func formatGigabytes(bytes float64) string {
	gb := bytes / (1000 * 1000 * 1000)
	return fmt.Sprintf("%.0fGB", gb)
}


func updateNetDiskPrometheus(netdiskMetrics NetDiskMetrics) {
	networkActivity.With(prometheus.Labels{"type": "in"}).Set(float64( ( netdiskMetrics.InBytesPerSec * 1000 ) / ( 1024 * 1024 ) ))
	networkActivity.With(prometheus.Labels{"type": "out"}).Set(float64( ( netdiskMetrics.OutBytesPerSec * 1000 ) / ( 1024 * 1024 ) ))
	diskActivity.With(prometheus.Labels{"type": "read"}).Set(float64( ( netdiskMetrics.ReadKBytesPerSec * 1000 ) / ( 1024 * 1024 ) ))
	diskActivity.With(prometheus.Labels{"type": "write"}).Set(float64( ( netdiskMetrics.WriteKBytesPerSec * 1000 ) / ( 1024 * 1024 ) ))
}

func parseCPUMetrics(data map[string]interface{}, cpuMetrics CPUMetrics) CPUMetrics {
	processor, ok := data["processor"].(map[string]interface{})
	if !ok {
		stderrLogger.Fatalf("Failed to get processor data\n")
		return cpuMetrics
	}

	thermal, ok := data["thermal_pressure"].(string)
	if !ok {
		stderrLogger.Fatalf("Failed to get thermal data\n")
	}

	cpuMetrics.Throttled = thermal != "Nominal"

	eCores := []int{}
	pCores := []int{}
	cpuMetrics.ECores = eCores
	cpuMetrics.PCores = pCores

	if cpuEnergy, ok := processor["cpu_power"].(float64); ok {
		cpuMetrics.CPUW = float64(cpuEnergy) / 1000
	}
	if gpuEnergy, ok := processor["gpu_power"].(float64); ok {
		cpuMetrics.GPUW = float64(gpuEnergy) / 1000
	}
	if combinedPower, ok := processor["combined_power"].(float64); ok {
		cpuMetrics.PackageW = float64(combinedPower) / 1000
	}

	return cpuMetrics
}

func max(nums ...int) int {
	maxVal := nums[0]
	for _, num := range nums[1:] {
		if num > maxVal {
			maxVal = num
		}
	}
	return maxVal
}

func getSOCInfo() map[string]interface{} {
	cpuInfoDict := getCPUInfo()
	coreCountsDict := getCoreCounts()
	var eCoreCounts, pCoreCounts int
	// hw.perflevel0 = Performance cores (P-cores)
	// hw.perflevel1 = Efficiency cores (E-cores)
	if val, ok := coreCountsDict["hw.perflevel1.logicalcpu"]; ok {
		eCoreCounts = val
	}
	if val, ok := coreCountsDict["hw.perflevel0.logicalcpu"]; ok {
		pCoreCounts = val
	}

	// Detect if this is an Ultra chip (two dies fused together)
	cpuName := cpuInfoDict["machdep.cpu.brand_string"]
	isUltra := strings.Contains(cpuName, "Ultra")

	socInfo := map[string]interface{}{
		"name":           cpuName,
		"core_count":     cpuInfoDict["machdep.cpu.core_count"],
		"cpu_max_power":  nil,
		"gpu_max_power":  nil,
		"cpu_max_bw":     nil,
		"gpu_max_bw":     nil,
		"e_core_count":   eCoreCounts,
		"p_core_count":   pCoreCounts,
		"gpu_core_count": getGPUCores(),
		"is_ultra":       isUltra,
	}
	return socInfo
}

func getMemoryMetrics() MemoryMetrics {
	v, _ := mem.VirtualMemory()
	s, _ := mem.SwapMemory()
	totalMemory := v.Total
	usedMemory := v.Used
	availableMemory := v.Available
	swapTotal := s.Total
	swapUsed := s.Used
	return MemoryMetrics{
		Total:     totalMemory,
		Used:      usedMemory,
		Available: availableMemory,
		SwapTotal: swapTotal,
		SwapUsed:  swapUsed,
	}
}

func getCPUInfo() map[string]string {
	out, err := exec.Command("sysctl", "machdep.cpu").Output()
	if err != nil {
		stderrLogger.Fatalf("failed to execute getCPUInfo() sysctl command: %v", err)
	}
	cpuInfo := string(out)
	cpuInfoLines := strings.Split(cpuInfo, "\n")
	dataFields := []string{"machdep.cpu.brand_string", "machdep.cpu.core_count"}
	cpuInfoDict := make(map[string]string)
	for _, line := range cpuInfoLines {
		for _, field := range dataFields {
			if strings.Contains(line, field) {
				value := strings.TrimSpace(strings.Split(line, ":")[1])
				cpuInfoDict[field] = value
			}
		}
	}
	return cpuInfoDict
}

func getCoreCounts() map[string]int {
	cmd := exec.Command("sysctl", "hw.perflevel0.logicalcpu", "hw.perflevel1.logicalcpu")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := cmd.Output()
	if err != nil {
		stderrLogger.Fatalf("failed to execute getCoreCounts() sysctl command: %v", err)
	}
	coresInfo := string(out)
	coresInfoLines := strings.Split(coresInfo, "\n")
	dataFields := []string{"hw.perflevel0.logicalcpu", "hw.perflevel1.logicalcpu"}
	coresInfoDict := make(map[string]int)
	for _, line := range coresInfoLines {
		for _, field := range dataFields {
			if strings.Contains(line, field) {
				value, _ := strconv.Atoi(strings.TrimSpace(strings.Split(line, ":")[1]))
				coresInfoDict[field] = value
			}
		}
	}
	return coresInfoDict
}

func getGPUCores() string {
	cmd, err := exec.Command("system_profiler", "-detailLevel", "basic", "SPDisplaysDataType").Output()
	if err != nil {
		stderrLogger.Fatalf("failed to execute system_profiler command: %v", err)
	}
	output := string(cmd)
	stderrLogger.Printf("Output: %s\n", output)
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		if strings.Contains(line, "Total Number of Cores") {
			parts := strings.Split(line, ": ")
			if len(parts) > 1 {
				cores := strings.TrimSpace(parts[1])
				return cores
			}
			break
		}
	}
	return "?"
}
