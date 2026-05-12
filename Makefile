REPO := ghcr.io/ytsaurus

TARGET_OS := linux
TARGET_ARCH := amd64

RELEASE_VERSION ?= 0.0.0

ifdef DOCKER_CONTEXT
DOCKER_ARGS := --context $(DOCKER_CONTEXT)
else
DOCKER_ARGS :=
endif

BUILD_PLATFORM := $(TARGET_OS)/$(TARGET_ARCH)
IMAGE_TAG := $(REPO)/task-proxy:$(RELEASE_VERSION)
CHART_PACKAGE := task-proxy-chart-$(RELEASE_VERSION).tgz

.PHONY: test
test:
	@echo "🧪 Running tests..."
	cd server/pkg && go test ./... -v

.PHONY: build
build:
	@echo "⚙️  Building binary for $(BUILD_PLATFORM)..."
	cd server && \
	    GOOS=$(TARGET_OS) GOARCH=$(TARGET_ARCH) \
	    go build -o ../dist/task-proxy .
	@echo "✅ Binary built: dist/task-proxy"

.PHONY: image
image: build test
	@echo "🐳 Building Docker image: $(IMAGE_TAG)..."
	docker $(DOCKER_ARGS) build \
	    --platform $(BUILD_PLATFORM) \
	    -t $(IMAGE_TAG) \
	.
	@echo "✅ Image built: $(IMAGE_TAG)"

.PHONY: helm-chart
helm-chart: image
	@echo "📦 Packaging Helm chart version $(RELEASE_VERSION)..."
	helm package chart \
	    --version $(RELEASE_VERSION) \
	    --app-version $(RELEASE_VERSION) \
	    --destination .
	@echo "✅ Chart packaged: $(CHART_PACKAGE)"

.PHONY: release
release: helm-chart
	@echo "🚀 Performing release version $(RELEASE_VERSION)..."
	@echo "  → Pushing Docker image..."
	docker $(DOCKER_ARGS) push $(IMAGE_TAG)
	@echo "  → Pushing Helm chart..."
	helm push $(CHART_PACKAGE) oci://$(REPO)
	@echo "✅ Release completed: $(RELEASE_VERSION)"

.PHONY: clean
clean:
	@echo "🗑️  Cleaning up artifacts..."
	rm -rf dist/ $(CHART_PACKAGE)
	@echo "✅ Cleanup completed"

.DEFAULT_GOAL := helm-chart
