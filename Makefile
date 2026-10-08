# Checks that need a Linux kernel run in a container, so they work from a Mac too.
LAB := mtproto-lab:local
MODCACHE := $(shell go env GOMODCACHE)
RUN := docker run --rm -v "$(CURDIR)":/w -v "$(MODCACHE)":/go/pkg/mod -w /w

.PHONY: test tidy lab lab-image drill fullpath soak soak-agent agent

# The everyday gate.
test:
	@bad=$$(find . -name '*.go' -not -path './web/*' -print0 | xargs -0 gofmt -l); test -z "$$bad" || { echo "$$bad"; echo "gofmt needed"; exit 1; }
	go vet ./...
	GOOS=linux go vet ./...
	go test -race -count=1 ./...

tidy:
	go mod tidy -diff

lab-image:
	docker build -q -t $(LAB) lab

# The same tests on Linux, race detector on.
lab: lab-image
	$(RUN) -e CGO_ENABLED=1 $(LAB) go test -race -count=1 ./...

# Root-only drills in fresh network namespaces (nft, real sockets). The full-path
# drill fetches the pinned telemt into DRILL_CACHE once and reuses it.
DRILL_CACHE ?= $(HOME)/.cache/mtproto-drill
DRILL := $(RUN) --privileged -v "$(DRILL_CACHE)":/cache -e TELEMT_CACHE=/cache -e CGO_ENABLED=1 -e MTPROTO_NETNS_DRILL=1 $(LAB)
drill: lab-image
	mkdir -p "$(DRILL_CACHE)"
	$(DRILL) go test -race -count=1 -run 'Drill' -v ./...

# Only the full-path drill: real telemt behind the capture, link and hub.
fullpath: lab-image
	mkdir -p "$(DRILL_CACHE)"
	$(DRILL) go test -race -count=1 -run TestFullPathDrill -v ./drill

# Idle-stream footprint through the real link chain (SOAK_STREAMS=n changes the count).
soak:
	SOAK=1 go test -count=1 -run Soak -v ./link

# The agent binary in its own process, an hour by default (SOAK_DURATION, SOAK_STREAMS,
# SOAK_CHURN); the samples land in dist/ under a fresh name each run.
soak-agent: lab-image
	mkdir -p dist
	$(RUN) --privileged -e SOAK_FULL=1 -e SOAK_DURATION -e SOAK_STREAMS -e SOAK_CHURN -e SOAK_CSV=/w/dist/soak-agent-$$(date +%Y%m%d-%H%M%S).csv $(LAB) go test -count=1 -run TestSoakProcess -timeout 3h -v ./drill

# What a release builds (same flags as the release workflow and the pins check).
VERSION ?= 0.0.0-dev
agent:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags "-s -w -X main.version=$(VERSION)" -o dist/mtproto-agent-linux-amd64 ./cmd/mtproto-agent
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -buildvcs=false -ldflags "-s -w -X main.version=$(VERSION)" -o dist/mtproto-agent-linux-arm64 ./cmd/mtproto-agent
