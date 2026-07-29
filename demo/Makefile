.DEFAULT_GOAL := run
.PHONY: run

run:
	@trap 'kill 0' EXIT INT TERM; \
	GODEBUG=traceallocfree=1 go run . & \
	sleep 1; \
	(cd .. && go run .) & \
	echo "open http://127.0.0.1:8080  (Ctrl-C to stop)"; \
	wait
