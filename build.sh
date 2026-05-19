#!/bin/bash
set -euo pipefail
go build -o http2-obfuscator .
echo "Built: http2-obfuscator"