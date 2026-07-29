BIN := $(CURDIR)/bin

.DEFAULT_GOAL := run
.PHONY: build run stop logs clean

build:
	@mkdir -p $(BIN)
	go build -o $(BIN)/demo ./demo
	go build -o $(BIN)/bridge ./bridge

run: build stop
	GODEBUG=traceallocfree=1 $(BIN)/demo > $(BIN)/demo.log 2>&1 &
	sleep 1
	$(BIN)/bridge > $(BIN)/bridge.log 2>&1 &
	sleep 1
	@echo "open http://127.0.0.1:8080  (make logs / make stop)"

stop:
	@pkill -9 -x demo 2>/dev/null || true
	@pkill -9 -x bridge 2>/dev/null || true

logs:
	tail -f $(BIN)/demo.log $(BIN)/bridge.log

clean: stop
	rm -rf $(BIN)
