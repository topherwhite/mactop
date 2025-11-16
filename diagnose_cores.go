//go:build ignore
// +build ignore

// Copyright (c) 2024-2025 Carsen Klock under MIT License
// Diagnostic tool to determine E-core vs P-core ordering
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
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

type CPUUsage struct {
	User   float64
	System float64
	Idle   float64
	Nice   float64
}

var (
	lastCPUTimes []CPUUsage
	firstRun     = true
)

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

func main() {
	if os.Geteuid() != 0 {
		fmt.Println("This diagnostic tool requires sudo privileges")
		fmt.Println("Usage: sudo go run diagnose_cores.go")
		os.Exit(1)
	}

	fmt.Println("=== Apple Silicon Core Topology Diagnostic ===\n")

	// Get system info
	cmd := exec.Command("sysctl", "-a")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := cmd.Output()
	if err != nil {
		fmt.Printf("Error running sysctl: %v\n", err)
		os.Exit(1)
	}

	output := string(out)
	lines := strings.Split(output, "\n")

	fmt.Println("Performance Levels:")
	for _, line := range lines {
		if strings.Contains(line, "hw.perflevel") &&
		   (strings.Contains(line, ".name") ||
		    strings.Contains(line, ".logicalcpu") ||
		    strings.Contains(line, ".physicalcpu")) {
			fmt.Println("  " + line)
		}
	}

	fmt.Println("\nTotal Core Count:")
	for _, line := range lines {
		if strings.Contains(line, "machdep.cpu.core_count") {
			fmt.Println("  " + line)
		}
	}

	// Get perflevel info
	perfLevel0 := 0
	perfLevel1 := 0
	perfLevel0Name := ""
	perfLevel1Name := ""

	for _, line := range lines {
		if strings.Contains(line, "hw.perflevel0.logicalcpu:") {
			parts := strings.Split(line, ":")
			if len(parts) > 1 {
				perfLevel0, _ = strconv.Atoi(strings.TrimSpace(parts[1]))
			}
		}
		if strings.Contains(line, "hw.perflevel1.logicalcpu:") {
			parts := strings.Split(line, ":")
			if len(parts) > 1 {
				perfLevel1, _ = strconv.Atoi(strings.TrimSpace(parts[1]))
			}
		}
		if strings.Contains(line, "hw.perflevel0.name:") {
			parts := strings.Split(line, ":")
			if len(parts) > 1 {
				perfLevel0Name = strings.TrimSpace(parts[1])
			}
		}
		if strings.Contains(line, "hw.perflevel1.name:") {
			parts := strings.Split(line, ":")
			if len(parts) > 1 {
				perfLevel1Name = strings.TrimSpace(parts[1])
			}
		}
	}

	// Get CPU name to detect Ultra chips
	cpuName := ""
	for _, line := range lines {
		if strings.Contains(line, "machdep.cpu.brand_string:") {
			parts := strings.Split(line, ":")
			if len(parts) > 1 {
				cpuName = strings.TrimSpace(parts[1])
			}
		}
	}
	isUltra := strings.Contains(cpuName, "Ultra")

	fmt.Printf("\n=== Interpretation ===\n")
	fmt.Printf("CPU: %s\n", cpuName)
	fmt.Printf("Is Ultra chip: %v\n", isUltra)
	fmt.Printf("perflevel0: %s cores (%d logical CPUs)\n", perfLevel0Name, perfLevel0)
	fmt.Printf("perflevel1: %s cores (%d logical CPUs)\n", perfLevel1Name, perfLevel1)

	// Now let's monitor CPU usage to see which cores are which
	fmt.Println("\n=== CPU Usage Test ===")
	fmt.Println("Monitoring CPU usage across all cores...")
	fmt.Println("Collecting data for 3 seconds...\n")

	// Get baseline CPU usage (first run returns zeros)
	_, err = GetCPUPercentages()
	if err != nil {
		fmt.Printf("Error getting baseline CPU usage: %v\n", err)
		os.Exit(1)
	}

	// Wait a moment
	time.Sleep(3 * time.Second)

	// Get current CPU usage
	currentUsage, err := GetCPUPercentages()
	if err != nil {
		fmt.Printf("Error getting current CPU usage: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Core ordering in CPU usage array (total: %d cores):\n", len(currentUsage))
	for i, usage := range currentUsage {
		fmt.Printf("  Core %2d: %6.2f%%\n", i, usage)
	}

	// Provide analysis
	fmt.Println("\n=== Analysis ===")
	if perfLevel0Name == "Performance" && perfLevel1Name == "Efficiency" {
		fmt.Printf("System has %d P-cores (Performance) and %d E-cores (Efficiency)\n",
			perfLevel0, perfLevel1)

		if isUltra {
			fmt.Println("\n*** ULTRA CHIP DETECTED ***")
			fmt.Println("Ultra chips have two dies fused together.")

			// Detect M3 vs M1/M2 Ultra
			isM3 := strings.Contains(cpuName, "M3")

			if isM3 {
				fmt.Println("\nM3 Ultra detected - E-cores FIRST within each die, then P-cores:")
				eCoresPerDie := perfLevel1 / 2
				pCoresPerDie := perfLevel0 / 2
				fmt.Printf("Expected pattern (2 dies, %d E-cores + %d P-cores per die):\n",
					eCoresPerDie, pCoresPerDie)
				fmt.Printf("  Die 1: E-cores at indices 0-%d, P-cores at indices %d-%d\n",
					eCoresPerDie-1, eCoresPerDie, eCoresPerDie+pCoresPerDie-1)
				die2EStart := eCoresPerDie + pCoresPerDie
				die2EEnd := die2EStart + eCoresPerDie - 1
				die2PStart := die2EEnd + 1
				die2PEnd := die2PStart + pCoresPerDie - 1
				fmt.Printf("  Die 2: E-cores at indices %d-%d, P-cores at indices %d-%d\n",
					die2EStart, die2EEnd, die2PStart, die2PEnd)

				// Analyze each die's pattern
				if len(currentUsage) >= perfLevel0+perfLevel1 {
					fmt.Println("\nDie 1 analysis:")
					die1EAvg := 0.0
					for i := 0; i < eCoresPerDie; i++ {
						die1EAvg += currentUsage[i]
					}
					die1EAvg /= float64(eCoresPerDie)
					fmt.Printf("  Die 1 E-cores (0-%d) average: %.2f%%\n", eCoresPerDie-1, die1EAvg)

					die1PAvg := 0.0
					for i := eCoresPerDie; i < eCoresPerDie+pCoresPerDie; i++ {
						die1PAvg += currentUsage[i]
					}
					die1PAvg /= float64(pCoresPerDie)
					fmt.Printf("  Die 1 P-cores (%d-%d) average: %.2f%%\n",
						eCoresPerDie, eCoresPerDie+pCoresPerDie-1, die1PAvg)

					fmt.Println("\nDie 2 analysis:")
					die2EAvg := 0.0
					for i := die2EStart; i <= die2EEnd && i < len(currentUsage); i++ {
						die2EAvg += currentUsage[i]
					}
					die2EAvg /= float64(eCoresPerDie)
					fmt.Printf("  Die 2 E-cores (%d-%d) average: %.2f%%\n", die2EStart, die2EEnd, die2EAvg)

					die2PAvg := 0.0
					for i := die2PStart; i <= die2PEnd && i < len(currentUsage); i++ {
						die2PAvg += currentUsage[i]
					}
					die2PAvg /= float64(pCoresPerDie)
					fmt.Printf("  Die 2 P-cores (%d-%d) average: %.2f%%\n", die2PStart, die2PEnd, die2PAvg)

					totalEAvg := (die1EAvg + die2EAvg) / 2.0
					totalPAvg := (die1PAvg + die2PAvg) / 2.0
					fmt.Printf("\nOverall E-cores average: %.2f%%\n", totalEAvg)
					fmt.Printf("Overall P-cores average: %.2f%%\n", totalPAvg)
				}
			} else {
				fmt.Println("\nM1/M2 Ultra detected - Uses INTERLEAVED topology:")
				pCoresPerDie := perfLevel0 / 2
				eCoresPerDie := perfLevel1 / 2
				fmt.Printf("Expected pattern (2 dies, %d P-cores + %d E-cores per die):\n",
					pCoresPerDie, eCoresPerDie)
				fmt.Printf("  Die 1: P-cores at indices 0-%d, E-cores at indices %d-%d\n",
					pCoresPerDie-1, pCoresPerDie, pCoresPerDie+eCoresPerDie-1)
				die2PStart := pCoresPerDie + eCoresPerDie
				die2PEnd := die2PStart + pCoresPerDie - 1
				die2EStart := die2PEnd + 1
				die2EEnd := die2EStart + eCoresPerDie - 1
				fmt.Printf("  Die 2: P-cores at indices %d-%d, E-cores at indices %d-%d\n",
					die2PStart, die2PEnd, die2EStart, die2EEnd)

				// Analyze each die's pattern
				if len(currentUsage) >= perfLevel0+perfLevel1 {
					fmt.Println("\nDie 1 analysis:")
					die1PAvg := 0.0
					for i := 0; i < pCoresPerDie; i++ {
						die1PAvg += currentUsage[i]
					}
					die1PAvg /= float64(pCoresPerDie)
					fmt.Printf("  Die 1 P-cores (0-%d) average: %.2f%%\n", pCoresPerDie-1, die1PAvg)

					die1EAvg := 0.0
					for i := pCoresPerDie; i < pCoresPerDie+eCoresPerDie; i++ {
						die1EAvg += currentUsage[i]
					}
					die1EAvg /= float64(eCoresPerDie)
					fmt.Printf("  Die 1 E-cores (%d-%d) average: %.2f%%\n",
						pCoresPerDie, pCoresPerDie+eCoresPerDie-1, die1EAvg)

					fmt.Println("\nDie 2 analysis:")
					die2PAvg := 0.0
					for i := die2PStart; i <= die2PEnd && i < len(currentUsage); i++ {
						die2PAvg += currentUsage[i]
					}
					die2PAvg /= float64(pCoresPerDie)
					fmt.Printf("  Die 2 P-cores (%d-%d) average: %.2f%%\n", die2PStart, die2PEnd, die2PAvg)

					die2EAvg := 0.0
					for i := die2EStart; i <= die2EEnd && i < len(currentUsage); i++ {
						die2EAvg += currentUsage[i]
					}
					die2EAvg /= float64(eCoresPerDie)
					fmt.Printf("  Die 2 E-cores (%d-%d) average: %.2f%%\n", die2EStart, die2EEnd, die2EAvg)
				}
			}
		} else {
			fmt.Println("\nNon-Ultra chip detected.")
			fmt.Println("Expected orderings on Apple Silicon:")
			fmt.Printf("  Most common: P-cores FIRST (cores 0-%d), then E-cores (cores %d-%d)\n",
				perfLevel0-1, perfLevel0, perfLevel0+perfLevel1-1)

			// Analyze pattern
			if len(currentUsage) >= perfLevel0+perfLevel1 {
				pCoreAvg := 0.0
				for i := 0; i < perfLevel0; i++ {
					pCoreAvg += currentUsage[i]
				}
				pCoreAvg /= float64(perfLevel0)

				eCoreAvg := 0.0
				for i := perfLevel0; i < perfLevel0+perfLevel1; i++ {
					eCoreAvg += currentUsage[i]
				}
				eCoreAvg /= float64(perfLevel1)

				fmt.Printf("\nP-cores (0-%d) average: %.2f%%\n", perfLevel0-1, pCoreAvg)
				fmt.Printf("E-cores (%d-%d) average: %.2f%%\n", perfLevel0, perfLevel0+perfLevel1-1, eCoreAvg)
			}
		}
	}

	fmt.Println("\n=== Implementation Status ===")
	fmt.Println("The main.go code now handles all chip topologies:")
	fmt.Println("  - Non-Ultra: P-cores first (0 to P-1), E-cores second (P to P+E-1)")
	fmt.Println("  - M3 Ultra: E-cores first within each die (Die1_E, Die1_P, Die2_E, Die2_P)")
	fmt.Println("  - M1/M2 Ultra: P-cores first within each die (Die1_P, Die1_E, Die2_P, Die2_E)")
	fmt.Println("\nThe fix properly calculates separate averages for P-cores and E-cores.")
}
