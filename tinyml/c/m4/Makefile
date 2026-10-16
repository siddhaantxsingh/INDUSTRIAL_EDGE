# Bare-metal build of the edge classifier for QEMU MPS2 boards.
#   make CPU=cortex-m4   -> edge_m4.elf (mps2-an386, FPU not used by the kernel)
#   make CPU=cortex-m3   -> edge_m3.elf (mps2-an385)
CPU ?= cortex-m4
TAG := $(if $(filter cortex-m4,$(CPU)),m4,m3)
OUT ?= build
CC = arm-none-eabi-gcc
CFLAGS = -mcpu=$(CPU) -mthumb -O2 -std=c99 -Wall -Wextra -ffreestanding -fno-builtin -ffunction-sections -fdata-sections -fstack-usage
LDFLAGS = -nostdlib -T link.ld -Wl,--gc-sections -Wl,-Map=$(OUT)/edge_$(TAG).map
all: $(OUT)/edge_$(TAG).elf
$(OUT)/edge_$(TAG).elf: startup.c main.c ../edge_ml.c ../edge_ml.h ../model_data.h semihost.h link.ld
	mkdir -p $(OUT)/$(TAG)
	$(CC) $(CFLAGS) -c ../edge_ml.c -o $(OUT)/$(TAG)/edge_ml.o
	$(CC) $(CFLAGS) -c main.c -o $(OUT)/$(TAG)/main.o
	$(CC) $(CFLAGS) -c startup.c -o $(OUT)/$(TAG)/startup.o
	$(CC) $(CFLAGS) $(LDFLAGS) $(OUT)/$(TAG)/*.o -lgcc -o $@
	arm-none-eabi-size -A $(OUT)/$(TAG)/edge_ml.o
	arm-none-eabi-size $@
clean:
	rm -rf $(OUT)
