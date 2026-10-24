.PHONY: build test vet fmt-check check


CLI_BINARY ?= beam
VERSION_MODULE := github.com/Beam-Network/beam-cli-public/internal/version
CONFIG_MODULE := github.com/Beam-Network/beam-cli-public/internal/config
UPDATE_MODULE := github.com/Beam-Network/beam-cli-public/internal/update
LDFLAGS := $(strip \
	$(if $(BEAM_VERSION),-X $(VERSION_MODULE).Version=$(BEAM_VERSION)) \
	$(if $(BEAM_REGISTRY_URL),-X $(CONFIG_MODULE).DefaultRegistryURL=$(BEAM_REGISTRY_URL)) \
	$(if $(BEAM_AUTH_URL),-X $(CONFIG_MODULE).DefaultAuthURL=$(BEAM_AUTH_URL)) \
	$(if $(BEAM_API_URL),-X $(CONFIG_MODULE).DefaultAPIURL=$(BEAM_API_URL)) \
	$(if $(BEAM_COORDINATOR_URL),-X $(CONFIG_MODULE).DefaultCoordinatorURL=$(BEAM_COORDINATOR_URL)) \
	$(if $(BEAM_SALE_CHAIN_ID),-X $(CONFIG_MODULE).DefaultSaleChainID=$(BEAM_SALE_CHAIN_ID)) \
	$(if $(BEAM_SALE_RPC_URLS),-X $(CONFIG_MODULE).DefaultSaleRPCURLs=$(BEAM_SALE_RPC_URLS)) \
	$(if $(BEAM_SALE_ESCROW),-X $(CONFIG_MODULE).DefaultSaleEscrow=$(BEAM_SALE_ESCROW)) \
	$(if $(BEAM_CDN_BASE_URL),-X $(UPDATE_MODULE).DefaultBaseURL=$(BEAM_CDN_BASE_URL)))

build:
	go build $(if $(LDFLAGS),-ldflags "$(LDFLAGS)") -o $(CLI_BINARY) ./cmd/beam

test:
	go test ./...

vet:
	go vet ./...

fmt-check:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)

check: fmt-check test vet
