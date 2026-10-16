# Build for a real STM32 Cortex-M4 board (run from tinyml/board after
# `python3 tinyml/board/gen_vectors.py` from the project root).
CC = arm-none-eabi-gcc
CFLAGS = -mcpu=cortex-m4 -mthumb -O2 -std=c99 -Wall -Wextra -ffreestanding -fno-builtin -ffunction-sections -fdata-sections
build/edge_board.elf: main_board.c ../c/edge_ml.c ../c/m4/startup.c link_stm32.ld build/vectors.h
	$(CC) $(CFLAGS) -nostdlib -T link_stm32.ld -Wl,--gc-sections main_board.c ../c/edge_ml.c ../c/m4/startup.c -lgcc -o $@
	arm-none-eabi-size $@
