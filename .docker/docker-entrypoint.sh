#!/bin/sh
set -e

# docker-entrypoint.sh — Validate Oathkeeper configuration before starting.
#
# If the first argument is "serve", we run "oathkeeper validate" first to
# catch configuration errors early with clear diagnostics. If validation
# fails the container exits immediately instead of running with zero rules.

# Find the config file from the arguments
CONFIG_FILE=""
for i in "$@"; do
  case "$i" in
    -c) shift_next=1 ;;
    *)
      if [ "$shift_next" = "1" ]; then
        CONFIG_FILE="$i"
        shift_next=""
      fi
      ;;
  esac
done

case "$1" in
  serve)
    if [ -n "$CONFIG_FILE" ]; then
      echo "==> Running pre-flight configuration validation..."
      if ! oathkeeper validate -c "$CONFIG_FILE"; then
        echo "==> ❌ Configuration validation failed. Oathkeeper will NOT start."
        echo "==> Fix the errors above and try again."
        exit 1
      fi
      echo "==> ✅ Configuration validated successfully."
    fi
    ;;
esac

exec oathkeeper "$@"
