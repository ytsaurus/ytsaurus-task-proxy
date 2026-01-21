SERVER_VERSION=${shell cat server/version.txt}
CHART_VERSION=${shell cat chart/Chart.yaml | grep version | cut -f 2 -d : | tr -d ' '}
IAM_TOKEN=${shell ycp --profile prod iam create-token}

REPO=cr.yandex/crp4sbkc7nqdhb4c3q1b/ytsaurus

TARGET_OS=linux
TARGET_ARCH=amd64

.PHONY: all
all: compile-server build-server package-chart

.PHONY: compile-server
compile-server:
	@echo "Compiling task proxy server for target $(TARGET_OS)/$(TARGET_ARCH)"
	cd server && GOOS=$(TARGET_OS) GOARCH=$(TARGET_ARCH) go build . && cd ..

.PHONY: build-server
build-server:
	@echo "Building task proxy server docker"
	docker build --platform $(TARGET_OS)/$(TARGET_ARCH) . -t $(REPO)/task-proxy-server:$(SERVER_VERSION)

.PHONY: package-chart
package-chart:
	@echo "Package task proxy helm chart, version $(CHART_VERSION)"
	helm package chart --version $(CHART_VERSION)

.PHONY: push-server
push-server:
	@echo "Pushing task proxy server docker"
	docker push $(REPO)/task-proxy-server:$(SERVER_VERSION)

.PHONY: push-chart
push-chart:
	@echo "Pushing task proxy helm chart"
	helm registry login cr.yandex -u iam -p $(IAM_TOKEN)
	helm push task-proxy-$(CHART_VERSION).tgz oci://$(REPO)

.PHONY: clean
clean:
	@echo "Cleaning files"
	rm task-proxy-$(CHART_VERSION).tgz server/server
