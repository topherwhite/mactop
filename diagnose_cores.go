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

	fmt.Printf("\n=== Interpretation ===\n")
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
		fmt.Println("\nCommon orderings on Apple Silicon:")
		fmt.Println("  Option 1: P-cores FIRST (cores 0-9), then E-cores (cores 10-13)")
		fmt.Printf("  Option 2: E-cores FIRST (cores 0-%d), then P-cores (cores %d-%d)\n",
			perfLevel1-1, perfLevel1, perfLevel1+perfLevel0-1)

		// Try to determine which is correct
		avgFirst := 0.0
		avgSecond := 0.0

		if len(currentUsage) >= perfLevel0 {
			for i := 0; i < perfLevel0; i++ {
				avgFirst += currentUsage[i]
			}
			avgFirst /= float64(perfLevel0)
		}

		if len(currentUsage) >= perfLevel0+perfLevel1 {
			for i := perfLevel0; i < perfLevel0+perfLevel1; i++ {
				avgSecond += currentUsage[i]
			}
			avgSecond /= float64(perfLevel1)
		}

		fmt.Printf("\nAverage usage of first %d cores: %.2f%%\n", perfLevel0, avgFirst)
		fmt.Printf("Average usage of next %d cores: %.2f%%\n", perfLevel1, avgSecond)

		fmt.Println("\n=== Current Code Assumption ===")
		fmt.Printf("The code in main.go:551-566 currently assumes:\n")
		fmt.Printf("  - E-cores are at indices 0-%d\n", perfLevel1-1)
		fmt.Printf("  - P-cores are at indices %d-%d\n", perfLevel1, perfLevel1+perfLevel0-1)
		fmt.Println("\nThis is likely INCORRECT if P-cores come first in the array!")
	}

	fmt.Println("\n=== Recommendation ===")
	fmt.Println("To properly fix this, we need to:")
	fmt.Println("1. Determine the actual core ordering (likely P-cores first)")
	fmt.Println("2. Update the loop indices in updateCPUPrometheus() accordingly")
	fmt.Println("3. Consider using sysctl hw.perflevels to verify core types")
}
