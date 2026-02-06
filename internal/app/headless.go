package app

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Note: strings is still needed for TrimPrefix in startPrometheusServer call

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
			// Use topology-aware core mapping
			topology := GetCoreTopology(sysInfo)

			var ecoreAvg, pcoreAvg float64
			if len(topology.PCoreIndices) > 0 {
				var pcoreSum float64
				for _, idx := range topology.PCoreIndices {
					if idx < len(percentages) {
						pcoreSum += percentages[idx]
					}
				}
				pcoreAvg = pcoreSum / float64(len(topology.PCoreIndices))
			}
			if len(topology.ECoreIndices) > 0 {
				var ecoreSum float64
				for _, idx := range topology.ECoreIndices {
					if idx < len(percentages) {
						ecoreSum += percentages[idx]
					}
				}
				ecoreAvg = ecoreSum / float64(len(topology.ECoreIndices))
			}

			// Update all prometheus metrics
			cpuUsage.Set(cpuUsagePercent)
			ecoreUsage.Set(ecoreAvg)
			pcoreUsage.Set(pcoreAvg)
			gpuUsage.Set(m.GPUActive)
			gpuFreqMHz.Set(float64(m.GPUFreqMHz))
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
			totalPowerGauge.Set(m.TotalPower)
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
