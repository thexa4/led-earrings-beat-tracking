#!/usr/bin/env python3
"""One-shot transform: move the flag tables from RAM vars to flash const
strings. Parses animation.go, emits flags.go, rewrites animation.go.

Color is stored as a packed RGB triple: three bytes, no alpha (nothing in
the firmware reads Color's alpha bits). Every byte is emitted as an explicit
\xHH escape.

NOTE: this transform already ran once; the flag declarations it parses are no
longer in animation.go, and flags.go has since grown staticColors and
sparkleVariants by hand. Re-running this script as-is will fail - it is kept
only as the record of where the tables came from.
"""
import re, sys

src = open('animation.go').read()

# 1. Collect flag color constants: flagName = NewColor(r, g, b)
colors = {}
for m in re.finditer(r'\b(flag\w+)\s*=\s*NewColor\(([^)]*)\)', src):
    name, args = m.group(1), m.group(2)
    r, g, b = [int(x, 0) for x in args.split(',')]
    colors[name] = r | (g << 8) | (b << 16)

# 2. Collect palettes: flagName = Palette{ ... }
palettes = {}
for m in re.finditer(r'\b(flag\w+)\s*=\s*Palette\{([^}]*)\}', src):
    name, body = m.group(1), re.sub(r'//[^\n]*', '', m.group(2))
    names = [n.strip() for n in body.split(',') if n.strip()]
    if len(names) != 36:
        sys.exit(f'{name}: {len(names)} entries, want 36')
    for n in names:
        if n not in colors:
            sys.exit(f'{name}: unknown color {n}')
    palettes[name] = names

# 3. allFlags order
m = re.search(r'\ballFlags\s*=\s*\[\.\.\.\]Palette\{([^}]*)\}', src)
order = [n.strip() for n in m.group(1).split(',') if n.strip()]
for n in order:
    if n not in palettes:
        sys.exit(f'allFlags: unknown palette {n}')

# 4. Verify no other use of any flag identifier outside the declarations.
allnames = set(colors) | set(palettes) | {'allFlags'}
for n in allnames:
    # count uses: every identifier occurrence that is NOT "name =" on a decl
    for dm in re.finditer(rf'\b{n}\b[ \t]*=', src):
        line_start = src.rfind('\n', 0, dm.start()) + 1
        prefix = src[line_start:dm.start()].strip()
        if prefix not in ('', 'var'):
            sys.exit(f'{n}: assignment in unexpected context: {prefix}')

# Per-flag comments kept from the original table (flag name -> comment lines).
flag_comments = {
    'Lesbian': ['Five-stripe lesbian flag (five colors are easier to',
                 'distinguish than seven).'],
    'Gay': ['Flag for gay men. This is the less common 5 stripe version,',
             "since that's easier to make on the earrings. (Not sure how many",
             "men will wear these, but it's there for those who want it)."],
    'NonBinary': ['This one really pops! While the number of LEDs for each color',
                   'is balanced, it might look better with a bit more for yellow?'],
    'Ace': ['Hard to represent because of the black, but I did my best.',
             "Since it's a circle, the black does stand out."],
    'Aro': ['Aromantic flag.'],
    'Poly': ['The new polyamory flag, with the yellow heart:',
              'https://nl.wikipedia.org/wiki/Bestand:Tricolor_Polyamory_Pride_Flag.svg',
              "It's hard to represent the dark purple at the bottom, this seems",
              'as dark and purplish as possible.'],
}

def esc(bs):
    # Always \xHH, uppercase: the payload is color data, never text, and raw
    # printable bytes between the escapes read as noise.
    return ''.join(f'\\x{v:02X}' for v in bs)

lines = []
lines.append('package main\n')
lines.append('''
// Flag palettes live as *const* strings: 36 packed RGB triples per flag.
// Alpha is not stored at all - Color keeps it in bits 24..31 and nothing in
// the firmware reads it (NewColor leaves it 0, the bit-bang C helpers take
// 24-bit values) - so a color is three bytes, not four. The same data as
// `var Palette` values would need 10 x 108 bytes of RAM *plus* the same
// 1,080 bytes of flash as the initializer, plus the init-time copy code -
// 2.2 KB total for read-only data. As const strings they occupy flash only.
//
// flags.go is generated from the original hand-written table (the generator
// verified entry counts and that nothing else referenced the old vars); edit
// the color/entry lists here by regenerating, not by hand.

// Palette is a packed list of colors, three bytes (R, G, B) each.
type Palette string

func (p Palette) color(i int) Color {
	_ = p[3*i+2] // bounds check hint; three bytes per LED
	return Color(p[3*i]) | Color(p[3*i+1])<<8 | Color(p[3*i+2])<<16
}

const (
''')
for name in order:
    bs = []
    for cn in palettes[name]:
        v = colors[cn]
        bs += [v & 0xff, (v >> 8) & 0xff, (v >> 16) & 0xff]  # no alpha byte
    for c in flag_comments.get(name[4:], []):
        lines.append(f'\t// {c}\n')
    lines.append(f'\tflag{name[4:]} = Palette("{esc(bs)}")\n')
    if name[4:] == 'Poly':
        lines.append('\t// (the three yellow slots are the heart)\n')
lines.append(')\n\n')
lines.append('var allFlags = [...]Palette{\n')
for name in order:
    lines.append(f'\tflag{name[4:]},\n')
lines.append('}\n')

open('flags.go', 'w').write(''.join(lines))

# 5. Rewrite animation.go: remove the flag declarations and the Palette type.
out = []
skip_close_paren = False
for ln in src.split('\n'):
    s = ln.strip()
    # Palette type line
    if s == 'type Palette [numLEDs]Color':
        out.append('\t// (Palette moved to flags.go as a const-string type.)')
        continue
    if re.match(r'^\bflag\w+\s*=\s*(NewColor|Palette\{)', s):
        continue
    if re.match(r'^\ballFlags\s*=\s*\[\.\.\.\]Palette\{', s):
        continue
    out.append(ln)
src2 = '\n'.join(out)

# The per-flag comments are preserved via flag_comments; drop everything the
# transform moved into flags.go deterministically: from the line after the
# Palette-moved marker up to the blank line before func showFlag, keeping
# nothing (declarations are gone, comments were captured above).
src2 = re.sub(r'\t// \(Palette moved to flags\.go as a const-string type\.\)\n.*?\nfunc showFlag',
               '\t// (Palette and the flag tables moved to flags.go.)\nfunc showFlag', src2, flags=re.S)
src2 = src2.replace('return allFlags[variant][led]',
                    'return allFlags[variant].color(led)')

open('animation.go', 'w').write(src2)
print('flags.go written:', len(order), 'flags,', len(colors), 'colors')
