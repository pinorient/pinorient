#!/bin/bash

# Config
REMOTE_HOST="root@pinorient.com"
REMOTE_PATH="/root/pinorient/pinorient"  # Where the binary lives on the server
SERVICE_NAME="pinorient.service"  # Your systemd service file name
BINARY_NAME="pinorient"  # Built binary name (matches Makefile APP_NAME)

# Flags (defaults)
DO_CIM=false
DO_RIM=false

# Parse arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        --cim)
            DO_CIM=true
            shift
            ;;
        --rim)
            DO_RIM=true
            shift
            ;;
        --help)
            echo "Usage: ./deploy.sh [OPTIONS]"
            echo "Options:"
            echo "  --cim     Run collections import after restart"
            echo "  --rim     Run records import after restart"
            echo "  --help    Show this help"
            exit 0
            ;;
        *)
            echo "Unknown option: $1"
            echo "Run './deploy.sh --help' for usage."
            exit 1
            ;;
    esac
done

# Deploy binary
rsync -azP -e ssh "./$BINARY_NAME" "$REMOTE_HOST:$REMOTE_PATH"
if [ $? -ne 0 ]; then
    echo "Rsync failed!"
    exit 1
fi

#Restart service
ssh -T "$REMOTE_HOST" << EOF
    # Restart the service to pick up the new binary
    sudo systemctl restart "$SERVICE_NAME"

    # Check status
    sudo systemctl status "$SERVICE_NAME" --no-pager -l

    # Conditional imports (flags expanded from local env)
    if [ "$DO_CIM" = true ]; then
        echo 'Running collections import...'
        cd $REMOTE_PATH && echo "y" | ./$BINARY_NAME import collections
    fi
    if [ "$DO_RIM" = true ]; then
        echo 'Running records import...'
        cd $REMOTE_PATH && echo "y" | ./$BINARY_NAME import records
    fi
EOF
if [ $? -ne 0 ]; then
    echo "Remote commands failed!"
    exit 1
fi

echo "Deployment complete!"
if [ "$DO_CIM" = true ]; then echo "Collections import executed."; fi
if [ "$DO_RIM" = true ]; then echo "Records import executed."; fi