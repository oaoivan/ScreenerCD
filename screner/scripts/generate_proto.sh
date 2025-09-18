#!/bin/bash

# This script generates Protobuf code from the .proto files located in the pkg/protobuf directory.

PROTOC=$(which protoc)

if [ -z "$PROTOC" ]; then
  echo "Error: protoc (Protocol Buffers compiler) is not installed."
  exit 1
fi

# Generate Go code from the Protobuf definitions
echo "Generating Go code from Protobuf definitions..."
$PROTOC --go_out=plugins=grpc:pkg/protobuf pkg/protobuf/arbitrage.proto

echo "Protobuf code generation completed."