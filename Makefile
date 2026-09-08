# Wraps the build, test and lint steps CI runs, plus the formatting sequence the
# style guide asks for on every change.

GO      ?= go
TOOLS   := fstool cuetool disctool 1steptool qictool
BINDIR  ?= bin
PKGS    := ./...

.PHONY: all
all: fmt vet lint test

.PHONY: build
build:
	$(GO) build $(PKGS)

## tools builds every command into $(BINDIR).
.PHONY: tools
tools:
	@mkdir -p $(BINDIR)
	@for t in $(TOOLS); do \
		echo "building $$t"; \
		$(GO) build -o $(BINDIR)/$$t ./cmd/$$t || exit 1; \
	done

.PHONY: test
test:
	$(GO) test $(PKGS)

.PHONY: cover
cover:
	$(GO) test -coverprofile=coverage.out $(PKGS)
	$(GO) tool cover -func=coverage.out | tail -1

## fuzz runs every fuzz target briefly. FUZZTIME overrides the budget.
FUZZTIME ?= 30s
.PHONY: fuzz
fuzz:
	@for pkg in ./extfs ./fat ./mbr ./apm ./img86f ./ddrescue ./hfs ./iso ./cdrom ./cuebin ./onestep ./qic ./internal/dbf; do \
		for fz in $$($(GO) test $$pkg -list 'Fuzz.*' | grep '^Fuzz'); do \
			echo "fuzzing $$pkg $$fz"; \
			$(GO) test $$pkg -run "^$$fz$$" -fuzz "^$$fz$$" -fuzztime=$(FUZZTIME) || exit 1; \
		done; \
	done

.PHONY: vet
vet:
	$(GO) vet $(PKGS)

## fmt applies the goimports/go fix sequence the style guide requires.
.PHONY: fmt
fmt:
	$(GO) fix $(PKGS)
	@if command -v goimports >/dev/null 2>&1; then \
		goimports -w .; \
	else \
		echo "goimports not installed; falling back to gofmt"; \
		gofmt -w .; \
	fi

## fmtcheck fails when anything is unformatted, for CI.
.PHONY: fmtcheck
fmtcheck:
	@out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then \
		echo "these files are not gofmt-clean:"; echo "$$out"; exit 1; \
	fi

.PHONY: lint
lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run $(PKGS); \
	else \
		echo "golangci-lint not installed; skipping"; \
	fi

## qemutest runs the CD-ROM tests inside a QEMU virtual machine, It needs
## qemu and qemu-test installed, so it is not part of the ci target.
.PHONY: qemutest
qemutest:
	@if ! command -v qemu-test >/dev/null 2>&1; then \
		echo "qemu-test not installed; skipping"; \
	elif ! command -v qemu-system-x86_64 >/dev/null 2>&1; then \
		echo "qemu-system-x86_64 not installed; skipping"; \
	else \
		$(GO) run ./internal/qemucd $(QEMUCDFLAGS); \
	fi

## crossbuild compiles the platform-specific code for every supported target.
## cdrom's darwin and linux ioctl paths are behind build tags, so neither is
## compiled by a plain build on the other platform.
.PHONY: crossbuild
crossbuild:
	GOOS=darwin  GOARCH=arm64 $(GO) build $(PKGS)
	GOOS=darwin  GOARCH=amd64 $(GO) build $(PKGS)
	GOOS=linux   GOARCH=amd64 $(GO) build $(PKGS)
	GOOS=linux   GOARCH=arm64 $(GO) build $(PKGS)
	GOOS=windows GOARCH=amd64 $(GO) build $(PKGS)

.PHONY: crossvet
crossvet:
	GOOS=darwin $(GO) vet $(PKGS)
	GOOS=linux  $(GO) vet $(PKGS)

## ci runs everything the CI workflow runs.
.PHONY: ci
ci: fmtcheck build vet crossbuild crossvet lint test

.PHONY: clean
clean:
	rm -rf $(BINDIR) coverage.out
