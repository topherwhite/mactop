package app

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func runHeadless(count int) {
	if err := initSocMetrics(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize metrics: %v\n", err)
		os.Exit(1)
	}
	defer cleanupSocMetrics()

	if prometheusPort != "" {
		// Use startPrometheusServer to register custom mactop metrics
		// Strip leading colon if present (CLI passes ":9090" but startPrometheusServer expects "9090")
		port := strings.TrimPrefix(prometheusPort, ":")
		startPrometheusServer(port)
	}

	ticker := time.NewTicker(time.Duration(updateInterval) * time.Millisecond)
	defer ticker.Stop()

	type HeadlessOutput struct {
		Timestamp    string         `json:"timestamp"`
		SocMetrics   SocMetrics     `json:"soc_metrics"`
		Memory       MemoryMetrics  `json:"memory"`
		NetDisk      NetDiskMetrics `json:"net_disk"`
		CPUUsage     float64        `json:"cpu_usage"`
		GPUUsage     float64        `json:"gpu_usage"`
		CoreUsages   []float64      `json:"core_usages"`
		SystemInfo   SystemInfo     `json:"system_info"`
		ThermalState string         `json:"thermal_state"`
		CPUTemp      float32        `json:"cpu_temp"`
		GPUTemp      float32        `json:"gpu_temp"`
	}

	encoder := json.NewEncoder(os.Stdout)

	GetCPUPercentages()

	if count > 0 {
		fmt.Print("[")
	}

	samplesCollected := 0
	for range ticker.C {
		m := sampleSocMetrics(updateInterval)
		mem := getMemoryMetrics()
		netDisk := getNetDiskMetrics()

		var cpuUsagePercent float64
		percentages, err := GetCPUPercentages()
		if err == nil && len(percentages) > 0 {
			var total float64
			for _, p := range percentages {
				total += p
			}
			cpuUsagePercent = total / float64(len(percentages))
		}

		thermalStr, _ := getThermalStateString()

		componentSum := m.TotalPower
		totalPower := m.SystemPower

		if totalPower < componentSum {
			totalPower = componentSum
		}

		residualSystem := totalPower - componentSum

		m.SystemPower = residualSystem
		m.TotalPower = totalPower

		sysInfo := getSOCInfo()

		output := HeadlessOutput{
			Timestamp:    time.Now().Format(time.RFC3339),
			SocMetrics:   m,
			Memory:       mem,
			NetDisk:      netDisk,
			CPUUsage:     cpuUsagePercent,
			GPUUsage:     m.GPUActive,
			CoreUsages:   percentages,
			SystemInfo:   sysInfo,
			ThermalState: thermalStr,
			CPUTemp:      m.CPUTemp,
			GPUTemp:      m.GPUTemp,
		}

		// Update Prometheus metrics
		if prometheusPort != "" && len(percentages) > 0 {
			// Calculate E-core and P-core averages using correct topology
			var ecoreAvg, pcoreAvg float64
			pCoreCount := sysInfo.PCoreCount
			eCoreCount := sysInfo.ECoreCount

			// Check if M1/M2 Ultra (interleaved topology)
			isInterleaved := sysInfo.IsUltra && (strings.Contains(sysInfo.Name, "M1") || strings.Contains(sysInfo.Name, "M2"))

			if isInterleaved {
				// M1/M2 Ultra: interleaved topology
				pCorePerDie := pCoreCount / 2
				eCorePerDie := eCoreCount / 2
				var pcoreSum, ecoreSum float64
				for die := 0; die < 2; die++ {
					pCoreStart := die * (pCorePerDie + eCorePerDie)
					eCoreStart := pCoreStart + pCorePerDie
					for i := 0; i < pCorePerDie && pCoreStart+i < len(percentages); i++ {
						pcoreSum += percentages[pCoreStart+i]
					}
					for i := 0; i < eCorePerDie && eCoreStart+i < len(percentages); i++ {
						ecoreSum += percentages[eCoreStart+i]
					}
				}
				if pCoreCount > 0 {
					pcoreAvg = pcoreSum / float64(pCoreCount)
				}
				if eCoreCount > 0 {
					ecoreAvg = ecoreSum / float64(eCoreCount)
				}
			} else {
				// Non-interleaved: P-cores first, then E-cores
				for i := 0; i < pCoreCount && i < len(percentages); i++ {
					pcoreAvg += percentages[i]
				}
				if pCoreCount > 0 {
					pcoreAvg /= float64(pCoreCount)
				}
				for i := 0; i < eCoreCount && pCoreCount+i < len(percentages); i++ {
					ecoreAvg += percentages[pCoreCount+i]
				}
				if eCoreCount > 0 {
					ecoreAvg /= float64(eCoreCount)
				}
			}

			// Update all prometheus metrics
			cpuUsage.Set(cpuUsagePercent)
			ecoreUsage.Set(ecoreAvg)
			pcoreUsage.Set(pcoreAvg)
			gpuUsage.Set(m.GPUActive)
			gpuFreqMHz.Set(float64(m.GPUFreqMHz))

			powerUsage.With(prometheus.Labels{"component": "cpu"}).Set(m.CPUPower)
			powerUsage.With(prometheus.Labels{"component": "gpu"}).Set(m.GPUPower)
			powerUsage.With(prometheus.Labels{"component": "ane"}).Set(m.ANEPower)
			powerUsage.With(prometheus.Labels{"component": "dram"}).Set(m.DRAMPower)
			powerUsage.With(prometheus.Labels{"component": "gpu_sram"}).Set(m.GPUSRAMPower)
			powerUsage.With(prometheus.Labels{"component": "system"}).Set(m.SystemPower)
			powerUsage.With(prometheus.Labels{"component": "total"}).Set(m.TotalPower)

			socTemp.Set(float64(m.CPUTemp))
			gpuTemp.Set(float64(m.GPUTemp))

			thermalStateNum := 0
			switch thermalStr {
			case "Moderate":
				thermalStateNum = 1
			case "Heavy":
				thermalStateNum = 2
			case "Critical":
				thermalStateNum = 3
			}
			thermalState.Set(float64(thermalStateNum))

			memoryUsage.With(prometheus.Labels{"type": "used"}).Set(float64(mem.Used) / 1024 / 1024 / 1024)
			memoryUsage.With(prometheus.Labels{"type": "total"}).Set(float64(mem.Total) / 1024 / 1024 / 1024)
			memoryUsage.With(prometheus.Labels{"type": "swap_used"}).Set(float64(mem.SwapUsed) / 1024 / 1024 / 1024)
			memoryUsage.With(prometheus.Labels{"type": "swap_total"}).Set(float64(mem.SwapTotal) / 1024 / 1024 / 1024)

			networkSpeed.With(prometheus.Labels{"direction": "upload"}).Set(netDisk.OutBytesPerSec)
			networkSpeed.With(prometheus.Labels{"direction": "download"}).Set(netDisk.InBytesPerSec)
			diskIOSpeed.With(prometheus.Labels{"operation": "read"}).Set(netDisk.ReadKBytesPerSec)
			diskIOSpeed.With(prometheus.Labels{"operation": "write"}).Set(netDisk.WriteKBytesPerSec)
			diskIOPS.With(prometheus.Labels{"operation": "read"}).Set(netDisk.ReadOpsPerSec)
			diskIOPS.With(prometheus.Labels{"operation": "write"}).Set(netDisk.WriteOpsPerSec)
		}

		if samplesCollected > 0 && count > 0 {
			fmt.Print(",")
		}

		if err := encoder.Encode(output); err != nil {
			fmt.Fprintf(os.Stderr, "Error encoding JSON: %v\n", err)
		}

		samplesCollected++
		if count > 0 && samplesCollected >= count {
			fmt.Println("]")
			return
		}
	}
}
