#!/bin/bash

# Config
REMOTE_HOST="root@pinorient.com"
REMOTE_PATH="/root/pinorient/pinorient"  # Where the binary lives on the server
SERVICE_NAME="pinorient.service"  # Your systemd service file name
BINARY_NAME="pinorient"  # Built binary name (matches Makefile APP_NAME)

# Deploy binary
rsync -azP -e ssh "./$BINARY_NAME" "$REMOTE_HOST:$REMOTE_PATH"
if [ $? -ne 0 ]; then
    echo "Rsync failed!"
    exit 1
fi

# Restart the service to pick up the new binary, then check status
ssh -T "$REMOTE_HOST" << EOF
    sudo systemctl restart "$SERVICE_NAME"
    sudo systemctl status "$SERVICE_NAME" --no-pager -l
EOF
if [ $? -ne 0 ]; then
    echo "Remote commands failed!"
    exit 1
fi

echo "Deployment complete!"