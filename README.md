# Earring firmware with beat tracking

Implements firmware for [led earrings](https://lectronz.com/products/earring-ring-rgb36v2). Based off stock firmware: https://github.com/aykevl/things/blob/main/earring-ring-rgb36v2

Firmware is LLM generated without supervision. I cannot guarantee the quality of this code. I have tested it manually and it works for my usecase.

## Flashing

Firmware in repo root contains three pre-programmed patterns you can replace. [heartbeat](earring-ring-rgb36v2/examples/sync/heartbeat.c) in custom slot 0, [carousel](earring-ring-rgb36v2/examples/sync/carousel.c) in slot 1, [drop](earring-ring-rgb36v2/examples/sync/drop.c) in slot 2.

`openocd -f interface/stlink.cfg -f target/stm32l0.cfg -c 'program path/to/firmware-sync.hex verify reset exit'`

## Custom beat tracking patterns

This firmware adds support for using the beat tracking variables from flashed patterns. More information can be found in the [sync program readme](earring-ring-rgb36v2/SYNC-PROGRAMS.md).

You'll need to paste the contents of [sync-sim.js](editor/sync-sim.js) into the devtools console when on the [LED animation editor](https://aykevl.nl/apps/led-editor/).

## License

Feel free to use this code however you wish (in other words, you can treat it as public domain). Especially for small parts I won't mind if you just copy it in your project. For larger pieces of code or if you copy a project entirely, I would appreciate attribution somewhere (but it's not required).

If you prefer having an actual license for legal reasons, you can use the [MIT license](https://opensource.org/license/mit/) - but again, the code is public domain so no need to actually use this:

```
Copyright 2023 Ayke van Laethem

Permission is hereby granted, free of charge, to any person obtaining a
copy of this software and associated documentation files (the
“Software”), to deal in the Software without restriction, including
without limitation the rights to use, copy, modify, merge, publish,
distribute, sublicense, and/or sell copies of the Software, and to
permit persons to whom the Software is furnished to do so, subject to
the following conditions:

The above copyright notice and this permission notice shall be included
in all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED “AS IS”, WITHOUT WARRANTY OF ANY KIND, EXPRESS
OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF
MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT.
IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY
CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION OF CONTRACT,
TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION WITH THE
SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.
```
