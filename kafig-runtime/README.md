# Kafig Runtime

## Install Toolchain

### macOS

```shell
# Add the WASI target
rustup target add wasm32-wasip1

# Install binaryen (provides wasm-opt)
brew install binaryen

# Download wasi-sdk — macOS version
# For Apple Silicon (M1/M2/M3):
wget https://github.com/WebAssembly/wasi-sdk/releases/download/wasi-sdk-24/\
wasi-sdk-24.0-arm64-macos.tar.gz
tar xf wasi-sdk-24.0-arm64-macos.tar.gz
sudo mv wasi-sdk-24.0 /opt/wasi-sdk

# For Intel Mac:
wget https://github.com/WebAssembly/wasi-sdk/releases/download/wasi-sdk-24/\
wasi-sdk-24.0-x86_64-macos.tar.gz
tar xf wasi-sdk-24.0-x86_64-macos.tar.gz
sudo mv wasi-sdk-24.0 /opt/wasi-sdk

# Install Wizer
cargo install wizer --all-features
```

### Linux

```shell
# Add the WASI target to your Rust toolchain
rustup target add wasm32-wasip1

# Download wasi-sdk (provides the C compiler for QuickJS's C code)
# Check https://github.com/WebAssembly/wasi-sdk/releases for latest version
wget https://github.com/WebAssembly/wasi-sdk/releases/download/wasi-sdk-24/\
wasi-sdk-24.0-x86_64-linux.tar.gz
tar xf wasi-sdk-24.0-x86_64-linux.tar.gz
sudo mv wasi-sdk-24.0 /opt/wasi-sdk

# Optional but recommended: wasm-opt for size reduction
# Install via binaryen — available in most package managers
sudo apt install binaryen        # Ubuntu/Debian

# Install Wizer
cargo install wizer --all-features
```

## Build

Build the Rust runtime, optimize it and run wizer to initialize it.

```shell
make wizer
```

## Build & Copy into kafig-go

Copy the runtime WASM file into the kafig-go package for embedding.

```shell
make install
```
