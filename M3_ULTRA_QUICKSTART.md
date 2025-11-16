# M3 Ultra Debugging - Quick Start Guide

## What This Is

This is a debugging package for fixing E-core/P-core detection issues on M3 Ultra chips. The code works on all other Apple Silicon chips but M3 Ultra has a different topology that we haven't been able to fix remotely.

## Quick Start (3 steps)

### 1. Transfer files to M3 Ultra

Copy this entire directory to your M3 Ultra machine.

### 2. Gather diagnostics

On the M3 Ultra, run:
```bash
cd /path/to/mactop
sudo bash gather_m3_diagnostics.sh
```

This will create a directory like `m3_ultra_diagnostics_20250116_143022/` with all diagnostic data.

### 3. Start Claude Code debugging session

In the mactop directory, start Claude Code and use this prompt:

```
I'm continuing debugging from a previous session on M3 Ultra core topology detection.

Context:
- We've been fixing E-core vs P-core detection in mactop
- The fix works on non-Ultra and M1/M2 Ultra chips
- M3 Ultra is STILL NOT WORKING despite implementing support
- Read M3_ULTRA_DEBUG_RESUME.md for full context

I just ran gather_m3_diagnostics.sh and have fresh diagnostic data in:
[paste the directory name, e.g., m3_ultra_diagnostics_20250116_143022/]

Please analyze the diagnostic outputs and fix the M3 Ultra core detection. You can directly test changes by building and running the code on this M3 Ultra system.
```

## What's Included

- **M3_ULTRA_DEBUG_RESUME.md** - Full context and debugging instructions
- **gather_m3_diagnostics.sh** - Automated diagnostic data collection
- **diagnose_cores.go** - Diagnostic tool to analyze core topology
- **main.go** - Main mactop code with current (broken) M3 Ultra support
- **CORE_TOPOLOGY_FIX.md** - Documentation of what's been fixed so far

## The Problem

M3 Ultra chips report E-cores and P-cores incorrectly. We've implemented logic to handle their non-interleaved topology, but something is still wrong. We need direct access to M3 Ultra hardware to test and debug.

## Current Status

✅ **Working:**
- M1, M2, M3, M4 (base models)
- M1 Pro, M2 Pro, M3 Pro, M4 Pro
- M1 Max, M2 Max, M3 Max, M4 Max
- M1 Ultra, M2 Ultra

❌ **Not Working:**
- M3 Ultra (all configurations: 28-core, 32-core, etc.)

## After the Fix

Once fixed, the Prometheus metrics will correctly report:
- `mactop_pcore_usage_percent` - Average P-core usage
- `mactop_ecore_usage_percent` - Average E-core usage

And logs in `/var/log/mactop.log` will show correct core assignments.

## Questions?

Read `M3_ULTRA_DEBUG_RESUME.md` for detailed debugging instructions and context.
