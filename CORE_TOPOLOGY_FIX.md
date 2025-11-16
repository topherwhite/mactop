# Apple Silicon Core Topology Fix

## Problem Summary

The mactop code was incorrectly calculating E-core and P-core CPU usage metrics because it made wrong assumptions about how cores are ordered in the CPU usage array.

## Root Cause

### Original (Incorrect) Assumption
The code assumed E-cores came first in the CPU usage array:
- E-cores: indices 0 to `eCoreCount-1`
- P-cores: indices `eCoreCount` to `eCoreCount+pCoreCount-1`

### Actual Core Ordering

#### Non-Ultra Chips (M1, M2, M3, M4, Pro/Max variants)
P-cores come FIRST, E-cores come AFTER:
- P-cores: indices 0 to `pCoreCount-1`
- E-cores: indices `pCoreCount` to `pCoreCount+eCoreCount-1`

**Example: M4 Pro (10 P-cores + 4 E-cores)**
```
Indices 0-9:   P-cores (Performance)
Indices 10-13: E-cores (Efficiency)
```

#### Ultra Chips (M1 Ultra, M2 Ultra)
Ultra chips are two dies fused together. E-cores appear in TWO CHUNKS:
- Die 1 P-cores: indices 0 to `pCoresPerDie-1`
- Die 1 E-cores: indices `pCoresPerDie` to `pCoresPerDie+eCoresPerDie-1`
- Die 2 P-cores: indices `pCoresPerDie+eCoresPerDie` to `pCoresPerDie+eCoresPerDie+pCoresPerDie-1`
- Die 2 E-cores: remaining indices

**Example: M1 Ultra (16 P-cores + 4 E-cores, 8+2 per die)**
```
Indices 0-7:   Die 1 P-cores
Indices 8-9:   Die 1 E-cores
Indices 10-17: Die 2 P-cores
Indices 18-19: Die 2 E-cores
```

## The Fix

### Detection
Added Ultra chip detection in `getSOCInfo()` (main.go:695):
```go
isUltra := strings.Contains(cpuName, "Ultra")
```

### Core Mapping
Updated `updateCPUPrometheus()` (main.go:548-621) to handle both cases:

**Non-Ultra chips:**
- Calculates P-core average from indices 0 to `pCoreCount-1`
- Calculates E-core average from indices `pCoreCount` to `pCoreCount+eCoreCount-1`

**Ultra chips:**
- Splits cores per die: `pCoresPerDie = pCoreCount / 2`, `eCoresPerDie = eCoreCount / 2`
- Calculates P-core average from both dies' P-cores
- Calculates E-core average from both dies' E-cores

## Verification

### Diagnostic Tool
Run `sudo go run diagnose_cores.go` to:
1. Detect if the system is an Ultra chip
2. Display the core topology
3. Show actual CPU usage per core
4. Verify the core mapping is correct

### Test Results (M4 Pro)
```
P-cores (0-9) average:   23.13% ✓ (active)
E-cores (10-13) average:  1.00% ✓ (mostly idle)
```

## Files Modified

1. **main.go**
   - `getSOCInfo()`: Added Ultra chip detection
   - `updateCPUPrometheus()`: Implemented dual-path logic for Ultra vs non-Ultra

2. **diagnose_cores.go**
   - Enhanced to detect and visualize Ultra chip topology
   - Shows per-die core analysis for Ultra chips

## Prometheus Metrics Impact

The fix ensures accurate reporting of:
- `mactop_pcore_usage_percent`: Average P-core usage
- `mactop_ecore_usage_percent`: Average E-core usage

Both metrics now correctly aggregate usage from the appropriate cores on both Ultra and non-Ultra chips.
