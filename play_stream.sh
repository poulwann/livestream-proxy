#!/bin/bash
if [ $# -eq 0 ]; then
    echo "Usage: $0 <stream_url> [max_retries] [initial_delay]"
    echo "Examples:"
    echo "  $0 http://localhost:8080/stream"
    echo "  $0 http://192.168.1.100:8080/stream 50 3"
    exit 1
fi

STREAM_URL="$1"
MAX_RETRIES="${2:-999}"  # Default to 999 if not provided
RETRY_DELAY="${3:-2}"    # Default to 2 seconds if not provided

echo "Stream URL: $STREAM_URL"
echo "Max retries: $MAX_RETRIES"
echo "Initial retry delay: $RETRY_DELAY seconds"
echo ""

for i in $(seq 1 $MAX_RETRIES); do
    echo "=== Attempt $i/$MAX_RETRIES - Starting FFplay ==="
    echo "$(date): Connecting to $STREAM_URL"
    
    ffplay -fflags +genpts+discardcorrupt+igndts \
           -reconnect 1 \
           -reconnect_at_eof 1 \
           -reconnect_streamed 1 \
           -reconnect_delay_max 5 \
           -timeout 10000000 \
           -analyzeduration 2000000 \
           -probesize 2000000 \
           -max_delay 500000 \
           -sync audio \
           -framedrop \
           -window_title "Stream Attempt $i - $STREAM_URL" \
           "$STREAM_URL"
    
    exit_code=$?
    echo "$(date): FFplay exited with code $exit_code"
    
    # Exit codes:
    # 0 = Normal exit (user closed)
    # 1 = General error
    # 255 = Network/connection error
    case $exit_code in
        0)
            echo "Stream ended normally (user exit)"
            break
            ;;
        1)
            echo "General error occurred"
            ;;
        255)
            echo "Network/connection error"
            ;;
        *)
            echo "Unknown error (exit code: $exit_code)"
            ;;
    esac
    
    if [ $i -lt $MAX_RETRIES ]; then
        echo "Restarting in $RETRY_DELAY seconds..."
        sleep $RETRY_DELAY
        
        # Exponential backoff (max 10 seconds)
        RETRY_DELAY=$(( RETRY_DELAY < 10 ? RETRY_DELAY + 1 : 10 ))
    fi
done

echo ""
echo "Stream playback ended after $i attempts"
echo "Final exit code: $exit_code"