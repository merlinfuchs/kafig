TARGET = wasm32-wasip1
RELEASE_DIR = target/$(TARGET)/release
BINARY = kafig-runtime
GO_WASM_PATH = ../kafig-go/internal/wasm/runtime.wasm

.PHONY: build optimize wizer clean

build:
	cargo build --target $(TARGET) --release

optimize: build
	wasm-opt -Os \
		$(RELEASE_DIR)/$(BINARY).wasm \
		-o $(BINARY)-opt.wasm

wizer: optimize
	wizer $(BINARY)-opt.wasm \
		--allow-wasi \
		--wasm-bulk-memory true \
		-o $(BINARY)-initialized.wasm
	wasm-opt -Os \
		$(BINARY)-initialized.wasm \
		-o $(BINARY)-final.wasm

install: wizer
	cp $(BINARY)-final.wasm $(GO_WASM_PATH)
	@echo "✓ updated $(GO_WASM_PATH)"

clean:
	cargo clean
	rm -f $(BINARY)-opt.wasm $(BINARY)-initialized.wasm $(BINARY)-final.wasm
