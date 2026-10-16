/* MPS2 AN385 (Cortex-M3) / AN386 (Cortex-M4): code in SSRAM1 at 0x0,
 * data in SSRAM2/3 at 0x20000000. Sizes are the board's; the footprint
 * actually used is reported by arm-none-eabi-size. */
MEMORY {
  FLASH (rx) : ORIGIN = 0x00000000, LENGTH = 4M
  RAM (rwx)  : ORIGIN = 0x20000000, LENGTH = 4M
}
ENTRY(reset_handler)
_estack = ORIGIN(RAM) + LENGTH(RAM);
SECTIONS {
  .text : {
    KEEP(*(.vectors))
    *(.text*) *(.rodata*)
    . = ALIGN(4);
  } > FLASH
  .ARM.exidx : { *(.ARM.exidx*) } > FLASH
  _sidata = LOADADDR(.data);
  .data : { _sdata = .; *(.data*) . = ALIGN(4); _edata = .; } > RAM AT > FLASH
  .bss (NOLOAD) : { _sbss = .; *(.bss*) *(COMMON) . = ALIGN(4); _ebss = .; } > RAM
}
