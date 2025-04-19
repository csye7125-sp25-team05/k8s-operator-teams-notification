#!/bin/bash

# Script to check Go version and clean the environment

# Check if Go is installed
if command -v go &> /dev/null; then
    # Print Go version
    echo "Current Go version:"
    go version
    
    # Check for existing go.mod and go.sum
    if [ -f "go.mod" ]; then
        echo -e "\nExisting go.mod found:"
        cat go.mod
        
        # Create backup of existing go.mod
        echo -e "\nCreating backup of existing go.mod to go.mod.bak"
        cp go.mod go.mod.bak
    else
        echo -e "\nNo go.mod found in current directory."
    fi
    
    # Clean up any previous module setup
    echo -e "\nCleaning up the environment..."
    rm -f go.mod go.sum
    
    # Create a new go.mod with a specific Go version
    echo -e "\nCreating a new go.mod file with Go 1.21..."
    cat > go.mod << EOF
module github.com/yourusername/namespace-change-notifier

go 1.21

require (
	k8s.io/apimachinery v0.28.4
	k8s.io/client-go v0.28.4
)
EOF
    
    echo -e "\nNew go.mod created:"
    cat go.mod
    
    # Initialize dependencies
    echo -e "\nInitializing dependencies..."
    go mod tidy
    
    echo -e "\nGo module setup complete."
else
    echo "Go is not installed on this system."
fi