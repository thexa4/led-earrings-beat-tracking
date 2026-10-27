MEMORY
{
  FLASH_TEXT (rx) : ORIGIN = 0x08000000, LENGTH = 32K - 128 - 1536*3
  /* The top 64 bytes are the sync data block for uploaded programs, at a
     fixed address (syncshare.go, SYNC-PROGRAMS.md): kept out of the region
     so nothing - not even the heap - is ever placed there. */
  RAM (xrw)       : ORIGIN = 0x20000000, LENGTH = 8K - 64
}

_stack_size = 2K;

INCLUDE "targets/arm.ld"
