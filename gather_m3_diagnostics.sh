#!/bin/bash
# M3 Ultra Diagnostic Data Gathering Script
# Run this on the M3 Ultra system to collect all needed debug info

set -e

echo "=== M3 Ultra Diagnostic Data Collection ==="
echo "Starting at $(date)"
echo ""

# Check if running as root
if [ "$EUID" -ne 0 ]; then
    echo "Please run with sudo: sudo bash gather_m3_diagnostics.sh"
    exit 1
fi

OUTPUT_DIR="m3_ultra_diagnostics_$(date +%Y%m%d_%H%M%S)"
mkdir -p "$OUTPUT_DIR"

echo "Output directory: $OUTPUT_DIR"
echo ""

# System information
echo "Collecting system information..."
echo "=== System Information ===" > "$OUTPUT_DIR/sysctl_info.txt"
sysctl machdep.cpu.brand_string >> "$OUTPUT_DIR/sysctl_info.txt"
sysctl hw.perflevel0.logicalcpu >> "$OUTPUT_DIR/sysctl_info.txt"
sysctl hw.perflevel1.logicalcpu >> "$OUTPUT_DIR/sysctl_info.txt"
sysctl hw.perflevel0.physicalcpu >> "$OUTPUT_DIR/sysctl_info.txt"
sysctl hw.perflevel1.physicalcpu >> "$OUTPUT_DIR/sysctl_info.txt"
sysctl hw.perflevel0.name >> "$OUTPUT_DIR/sysctl_info.txt"
sysctl hw.perflevel1.name >> "$OUTPUT_DIR/sysctl_info.txt"
sysctl machdep.cpu.core_count >> "$OUTPUT_DIR/sysctl_info.txt"
sysctl hw.nperflevels >> "$OUTPUT_DIR/sysctl_info.txt"
echo "" >> "$OUTPUT_DIR/sysctl_info.txt"
system_profiler SPHardwareDataType >> "$OUTPUT_DIR/sysctl_info.txt"

# Build diagnostic tool
echo "Building diagnostic tool..."
go build -o diagnose_cores diagnose_cores.go

# Run diagnostic tool
echo "Running diagnostic tool (this will take 3-5 seconds)..."
./diagnose_cores > "$OUTPUT_DIR/diagnose_cores_output.txt" 2>&1

# Build mactop
echo "Building mactop..."
go build -o mactop main.go

# Run mactop briefly
echo "Starting mactop for 10 seconds to collect logs..."
rm -f /var/log/mactop.log
./mactop --prometheus=9091 > "$OUTPUT_DIR/mactop_stdout.txt" 2>&1 &
MACTOP_PID=$!

# Wait for mactop to initialize and collect some data
sleep 10

# Stop mactop
kill $MACTOP_PID 2>/dev/null || true
wait $MACTOP_PID 2>/dev/null || true

# Copy logs
if [ -f /var/log/mactop.log ]; then
    cp /var/log/mactop.log "$OUTPUT_DIR/mactop.log"
else
    echo "Warning: /var/log/mactop.log not found" > "$OUTPUT_DIR/mactop.log"
fi

# Create a CPU activity snapshot
echo "Collecting CPU activity snapshot..."
echo "=== CPU Activity During Collection ===" > "$OUTPUT_DIR/cpu_activity.txt"
top -l 1 -n 10 -stats pid,command,cpu >> "$OUTPUT_DIR/cpu_activity.txt"

# Summary
echo ""
echo "=== Data Collection Complete ==="
echo "All files saved to: $OUTPUT_DIR/"
echo ""
echo "Files collected:"
ls -lh "$OUTPUT_DIR/"
echo ""
echo "Next steps:"
echo "1. Review the files in $OUTPUT_DIR/"
echo "2. Share this directory with Claude Code"
echo "3. Use the prompt in M3_ULTRA_DEBUG_RESUME.md to continue debugging"
echo ""
echo "Key file to check first: $OUTPUT_DIR/diagnose_cores_output.txt"
