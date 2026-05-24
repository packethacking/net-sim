# Build LinBPQ variants for NET/ROM analysis.
#
# Each target applies the improvement patch with different -D flags,
# then builds a tagged Docker image.
#
# Usage:
#   make baseline          # unmodified LinBPQ
#   make split-horizon     # split-horizon routing
#   make adaptive-interval # adaptive NODES interval
#   make measured-quality  # measured quality feedback
#   make combined          # all three together
#   make all-variants      # build everything

LINBPQ_SRC   ?= /tmp/LinBPQ
PATCH_FILE   := linbpq-patches/netrom-improvements.patch
REGISTRY     := linbpq-test

.PHONY: baseline split-horizon adaptive-interval measured-quality combined all-variants clean

baseline:
	cd $(LINBPQ_SRC) && git checkout -- L3Code.c 2>/dev/null; \
	cd $(LINBPQ_SRC) && docker build -t $(REGISTRY):baseline -f docker/Dockerfile .

split-horizon:
	cd $(LINBPQ_SRC) && git checkout -- L3Code.c 2>/dev/null; \
	cd $(LINBPQ_SRC) && patch -p0 < $(CURDIR)/$(PATCH_FILE); \
	cd $(LINBPQ_SRC) && sed -i 's/^all: CFLAGS = /all: CFLAGS = -DSPLIT_HORIZON /' makefile; \
	cd $(LINBPQ_SRC) && docker build -t $(REGISTRY):split-horizon -f docker/Dockerfile . ; \
	cd $(LINBPQ_SRC) && git checkout -- makefile

adaptive-interval:
	cd $(LINBPQ_SRC) && git checkout -- L3Code.c 2>/dev/null; \
	cd $(LINBPQ_SRC) && patch -p0 < $(CURDIR)/$(PATCH_FILE); \
	cd $(LINBPQ_SRC) && sed -i 's/^all: CFLAGS = /all: CFLAGS = -DADAPTIVE_INTERVAL /' makefile; \
	cd $(LINBPQ_SRC) && docker build -t $(REGISTRY):adaptive-interval -f docker/Dockerfile . ; \
	cd $(LINBPQ_SRC) && git checkout -- makefile

measured-quality:
	cd $(LINBPQ_SRC) && git checkout -- L3Code.c 2>/dev/null; \
	cd $(LINBPQ_SRC) && patch -p0 < $(CURDIR)/$(PATCH_FILE); \
	cd $(LINBPQ_SRC) && sed -i 's/^all: CFLAGS = /all: CFLAGS = -DMEASURED_QUALITY /' makefile; \
	cd $(LINBPQ_SRC) && docker build -t $(REGISTRY):measured-quality -f docker/Dockerfile . ; \
	cd $(LINBPQ_SRC) && git checkout -- makefile

combined:
	cd $(LINBPQ_SRC) && git checkout -- L3Code.c 2>/dev/null; \
	cd $(LINBPQ_SRC) && patch -p0 < $(CURDIR)/$(PATCH_FILE); \
	cd $(LINBPQ_SRC) && sed -i 's/^all: CFLAGS = /all: CFLAGS = -DSPLIT_HORIZON -DADAPTIVE_INTERVAL -DMEASURED_QUALITY /' makefile; \
	cd $(LINBPQ_SRC) && docker build -t $(REGISTRY):combined -f docker/Dockerfile . ; \
	cd $(LINBPQ_SRC) && git checkout -- makefile

all-variants: baseline split-horizon adaptive-interval measured-quality combined

clean:
	cd $(LINBPQ_SRC) && git checkout -- L3Code.c makefile 2>/dev/null
	-docker rmi $(REGISTRY):baseline $(REGISTRY):split-horizon $(REGISTRY):adaptive-interval $(REGISTRY):measured-quality $(REGISTRY):combined 2>/dev/null
