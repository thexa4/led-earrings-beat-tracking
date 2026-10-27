// Party sync data for the LED editor's preview.
//
// Sync patterns (see earring-ring-rgb36v2/SYNC-PROGRAMS.md) read the music
// data the earring keeps at 0x20001fc0. The editor's preview has no such
// block, so it only ever shows a pattern's fallback. This snippet fills the
// block in the preview with a fake track - a steady beat, a new section every
// few bars cycling the four color schemes - and adds a panel to change it.
//
// Use: open the editor (https://aykevl.nl/apps/led-editor/), open the browser
// console (F12), paste this whole file and press Enter. Chrome asks you to
// type "allow pasting" first. In Chrome you can also save it under Sources >
// Snippets and run it from there with Ctrl+Enter. The preview restarts with
// the sync data and keeps it on every recompile. Pasting it again only shows
// the panel again. The ✕ on the panel (or syncSim.uninstall()) puts the
// editor back as it was.
//
// From the console:
//
//   syncSim.set({bpm: 140, silent: true})   change the panel's settings
//   syncSim.override({bass: 255, 'bands[3]': 0, beats: 7})
//                                           hold any field at a value
//   syncSim.release('bass')                 follow the music again (no name: all)
//   syncSim.nextSection()                   jump to the next section now
//   syncSim.status                          the values the pattern saw last
//
// How it works: the editor runs patterns in a Cortex-M emulator inside a Web
// Worker (simulator.js). The snippet wraps window.Worker so that the
// simulator worker first runs workerMain below, which hooks the emulator,
// writes the block into its RAM and takes over the render loop to call the
// pattern on sync mode's timing. The emulator's stack starts at 0x20002000,
// right on top of the block, so workerMain also moves the stack down.
//
// The music is made up: the values have the right shapes (a kick on the beat,
// hats on the off-beats, breakdowns without a kick), but this is not the
// earring's analysis, and the emulator doesn't check the cycle budget.

(() => {
  'use strict';

  if (window.syncSim) {
    window.syncSim.show();
    return;
  }

  // The four color schemes in pattern order, 16 RGB colors each: PartyColors,
  // RainbowColors, ForestColors and OceanColors from the firmware's
  // noisePalettes.go.
  const PALETTES =
    '5500ab84007cb5004be5001be81700b84700ab7700abab00ab5500dd2200f2000ec2003e8f00715f00a12f00d00007f9' +
    'ff0000d52a00ab5500ab7f00abab0056d50000ff0000d52a00ab550056aa0000ff2a00d55500ab7f0081ab0055d5002b' +
    '006400006400556b2f006400008000228b226b8e230080002e8b5766cdaa32cd329acd3290ee907cfc0066cdaa228b22' +
    '19197000008b19197000008000008b0000cd2e8b570080805f9ea00000ff008b8b6495ed7fffd42e8b5700ffff87cefa';

  const DEFAULTS = {
    active: true,     // SYNC_ACTIVE; off shows the pattern as in a custom mode
    locked: true,     // SYNC_LOCKED, and beat_ms is set
    silent: false,    // SYNC_SILENT: no beats, all levels 0
    swapRB: false,    // the long press on the variant button
    breakdowns: true, // every other section has no kick
    bpm: 128,
    bars: 4,          // bars (of 4 beats) per section
    pattern: 'auto',  // 'auto': the next of 0..3 every section
    overrides: {},    // field name -> value held there
  };
  const STORAGE_KEY = 'sync-sim';

  // Runs inside the simulator worker, before simulator.js. It is turned into
  // source with toString(), so it must not use anything from outside itself.
  function workerMain(simulatorURL, initialConfig, paletteHex) {
    'use strict';

    const RAM = 0x2000_0000;
    const SYNC_ADDR = 0x2000_1fc0;    // struct sync_data, as on the earring
    const PALETTE_ADDR = 0x2000_1f00; // the four palettes, 48 bytes each
    const STACK_TOP = PALETTE_ADDR;   // the emulator's stack, below both
    const SYNC_MAGIC = 0x434e5953;
    const TICK_MS = 15.625;           // one analysis frame: 64 per second
    const MAX_CATCH_UP = 64;          // ticks; after a hidden tab, skip ahead

    // The layout of struct sync_data: name, offset, size.
    const FIELDS = [
      ['magic', 0, 4], ['version', 4, 1], ['flags', 5, 1], ['pattern', 6, 1],
      ['beat_phase', 7, 1], ['frame', 8, 4], ['beats', 12, 4],
      ['sections', 16, 4], ['beat_ms', 20, 2], ['level', 22, 1],
      ['bass', 23, 1], ['mid', 24, 1], ['high', 25, 1],
      ['section_age', 26, 1], ['beat_age', 27, 1],
    ];
    for (let i = 0; i < 8; i++) {
      FIELDS.push([`bands[${i}]`, 28 + i, 1]);
    }
    FIELDS.push(['hash', 36, 4], ['palette', 40, 4]);

    let cfg = initialConfig;
    const palettes = new Uint8Array(paletteHex.match(/../g).map(h => parseInt(h, 16)));
    const block = new DataView(new ArrayBuffer(44));
    let values = null;  // the fields in block, by name, for the panel
    let dirty = true;   // block changed since it was copied into the emulator
    let wasmMemory = null;
    let imagePtr = null; // the emulator's vector table: [0] is the initial SP
    let memPtr = null;   // the emulator's RAM, at RAM

    // The fake track. Ages count ticks and start out "long ago".
    const music = {
      frame: 0,
      beats: 0,
      sections: 0,
      beatInSection: 0,
      beatAge: Infinity,
      hatAge: Infinity,
      snareAge: Infinity,
      sectionAge: Infinity,
      nextSection: false,
      bands: [0, 0, 0, 0, 0, 0, 0, 0],
      rng: 0x2545f491,
    };

    // simulator.js fetches the emulator by a relative URL, which this Blob
    // worker would resolve against its blob: URL.
    const realFetch = self.fetch.bind(self);
    self.fetch = (url, options) => realFetch(new URL(url, simulatorURL), options);

    // Hook the emulator: note where its vector table and RAM are, and on
    // every reset (before every call into the pattern, setup included) move
    // the stack below the block and copy the block in.
    const realInstantiate = WebAssembly.instantiateStreaming;
    WebAssembly.instantiateStreaming = async (source, imports) => {
      const result = await realInstantiate(source, imports);
      const real = result.instance.exports;
      wasmMemory = real.memory;
      const exports = Object.assign({}, real, {
        machine_get_image(machine) {
          return imagePtr = real.machine_get_image(machine);
        },
        machine_get_mem(machine) {
          return memPtr = real.machine_get_mem(machine);
        },
        machine_reset(machine) {
          if (imagePtr !== null) {
            new DataView(wasmMemory.buffer).setUint32(imagePtr, STACK_TOP, true);
          }
          if (memPtr !== null && dirty) {
            copyBlock();
          }
          return real.machine_reset(machine);
        },
      });
      return { module: result.module, instance: { exports } };
    };

    function copyBlock() {
      const ram = new Uint8Array(wasmMemory.buffer, memPtr, SYNC_ADDR + 64 - RAM);
      ram.set(palettes, PALETTE_ADDR - RAM);
      ram.set(new Uint8Array(block.buffer), SYNC_ADDR - RAM);
      dirty = false;
    }

    // The beat period in ticks, whole ticks like the earring's beat tracker.
    function period() {
      return Math.max(1, Math.round(3840 / cfg.bpm));
    }

    function random() { // 0..1, xorshift32
      let x = music.rng;
      x ^= x << 13;
      x ^= x >>> 17;
      x ^= x << 5;
      music.rng = x >>> 0;
      return music.rng / 2 ** 32;
    }

    function mix32(x) { // the murmur3 finalizer: section number -> hash
      x = Math.imul(x ^ (x >>> 16), 0x85ebca6b);
      x = Math.imul(x ^ (x >>> 13), 0xc2b2ae35);
      return (x ^ (x >>> 16)) >>> 0;
    }

    function newSection() {
      music.sections++;
      music.sectionAge = 0;
      music.beatInSection = 0;
    }

    // Advance the track by one tick.
    function step() {
      const m = music;
      const per = period();
      m.frame++;
      m.beatAge++;
      m.hatAge++;
      m.snareAge++;
      m.sectionAge++;
      if (m.nextSection) {
        m.nextSection = false;
        newSection();
      }
      if (!cfg.silent) {
        if (m.beatAge >= per) {
          if (m.beatInSection >= cfg.bars * 4) {
            newSection();
          }
          m.beatAge = 0;
          m.beats++;
          m.beatInSection++;
          if ((m.beatInSection & 1) === 0) {
            m.snareAge = 0; // beats 2 and 4 of the bar
          }
        } else if (m.beatAge === per >> 1) {
          m.hatAge = 0; // the off-beat
        }
      }
      synth(per);
    }

    // The band levels for this tick, 0..255 like the earring's (each band
    // scaled to its own loudest moments).
    function synth(per) {
      const m = music;
      if (cfg.silent) {
        m.bands.fill(0);
        return;
      }
      const drop = !(cfg.breakdowns && (m.sections & 1));
      const kick = drop ? Math.exp(-m.beatAge / 5) : 0;
      const snare = drop ? Math.exp(-m.snareAge / 7) : 0;
      const hat = (drop ? 1 : 0.5) * Math.exp(-m.hatAge / 2.5);
      const bar = (((m.beatInSection - 1) & 3) + Math.min(m.beatAge / per, 1)) / 4;
      const swell = Math.sin(2 * Math.PI * bar);
      const pad = drop ? 0.25 + 0.1 * swell : 0.55 + 0.25 * swell;
      const sub = drop ? 0.3 : 0.15;
      const levels = [
        Math.max(kick, sub),          // < 250 Hz: kick, bass line
        0.5 * kick + pad,             // 250-400 Hz
        0.8 * snare + 0.9 * pad,      // 400-630 Hz
        0.9 * snare + 0.7 * pad,      // 630 Hz-1 kHz
        0.7 * snare + 0.3 * pad + 0.2 * hat, // 1-1.6 kHz
        0.6 * hat + 0.3 * snare,      // 1.6-2.5 kHz
        0.8 * hat + 0.2 * snare,      // 2.5-4 kHz
        hat,                          // > 4 kHz
      ];
      for (let i = 0; i < 8; i++) {
        const level = levels[i] + (random() - 0.5) * 0.06;
        m.bands[i] = Math.round(255 * Math.min(Math.max(level, 0), 1));
      }
    }

    // Work out the fields from the track, the settings and the held values,
    // and store them in block.
    function fill() {
      const m = music;
      const held = cfg.overrides;
      const per = period();
      const v = {
        magic: SYNC_MAGIC,
        version: 1,
        // Outside sync mode the firmware clears all flags.
        flags: cfg.active ? 1 | (cfg.locked ? 2 : 0) | (cfg.silent ? 4 : 0) : 0,
        pattern: cfg.pattern === 'auto' ? m.sections & 3 : cfg.pattern & 3,
        beat_phase: Math.min(Math.floor(256 * Math.min(m.beatAge, per) / per), 255),
        frame: m.frame,
        beats: m.beats,
        sections: m.sections,
        beat_ms: cfg.locked ? per * 125 >> 3 : 0,
      };
      for (let i = 0; i < 8; i++) {
        const name = `bands[${i}]`;
        v[name] = typeof held[name] === 'number' ? held[name] : m.bands[i];
      }
      const band = i => v[`bands[${i}]`];
      v.level = Math.min(255, Math.round((3 * band(0) + band(1) + band(2) + band(3) +
        band(4) + band(5) + band(6) + band(7)) * 0.16));
      // bass, mid and high from the bands as in the firmware's syncPoll.
      v.bass = band(0);
      v.mid = (band(1) + band(2) + band(3)) * 85 >> 8;
      v.high = (band(5) + band(6) + band(7)) * 85 >> 8;
      v.section_age = Math.min(m.sectionAge, 255);
      v.beat_age = Math.min(m.beatAge, 255);
      v.hash = mix32(m.sections + 0x9e3779b9);
      for (const name in held) {
        if (name in v && typeof held[name] === 'number') {
          v[name] = held[name];
        }
      }
      v.palette = PALETTE_ADDR + (v.pattern & 3) * 48;

      for (const [name, offset, size] of FIELDS) {
        if (size === 1) {
          block.setUint8(offset, v[name]);
        } else if (size === 2) {
          block.setUint16(offset, v[name], true);
        } else {
          block.setUint32(offset, v[name], true);
        }
      }
      values = v;
      dirty = true;
    }

    // Call a function of the pattern, the way simulator.js's render does.
    function call(name, addr, r0, r1 = 0) {
      const view = new DataView(engine.memory.buffer);
      view.setUint32(engine.imagePtr + 4, addr, true); // program counter
      view.setUint32(engine.machinePtr + 0, r0, true);  // R0: first param
      view.setUint32(engine.machinePtr + 4, r1, true);  // R1: second param
      engine.machine_reset(engine.machinePtr);
      const status = engine.machine_run(engine.machinePtr);
      if (status !== 0) {
        sendError(`could not run the ${name} function, error code: ${status}`);
        return null;
      }
      return view.getUint32(engine.machinePtr + 0, true); // R0: result value
    }

    let t0 = null; // when the sync preview started
    let lastTick = -1;

    // render() for sync mode: millis counts real time since the start in
    // ticks, and nextFrame runs 32 times a second, getPixel at every render.
    function syncRender() {
      const now = performance.now();
      if (t0 === null) {
        t0 = now;
      }
      const tick = Math.floor((now - t0) / TICK_MS);
      if (tick - lastTick > MAX_CATCH_UP) {
        music.frame += tick - lastTick - MAX_CATCH_UP;
        lastTick = tick - MAX_CATCH_UP;
      }
      // One tick at a time, so that no beat or section change is missed.
      while (lastTick < tick) {
        lastTick++;
        step();
        if ((lastTick & 1) === 0) {
          fill();
          if (call('nextFrame', engine.nextFrameAddr, Math.floor(lastTick * TICK_MS)) === null) {
            return;
          }
        }
      }
      fill();
      const millis = Math.floor(tick * TICK_MS);
      for (let index = 0; index < deviceInfo.num; index++) {
        let color = call('getPixel', engine.getPixelAddr, index, millis);
        if (color === null) {
          return;
        }
        if (cfg.swapRB) {
          color = (color & 0xff00ff00) | (color >>> 16 & 0xff) | (color & 0xff) << 16;
        }
        pixelBuf[index] = color;
      }
      postMessage({ type: 'pixelBuf', buf: pixelBuf, sync: values });
    }

    fill();
    importScripts(simulatorURL);

    const simulatorOnMessage = self.onmessage;
    self.onmessage = (e) => {
      if (e.data && e.data.type === 'syncsim') {
        if (e.data.cfg) {
          cfg = e.data.cfg;
        }
        if (e.data.cmd === 'section') {
          music.nextSection = true;
        }
        fill();
        return;
      }
      simulatorOnMessage(e);
    };

    if (typeof render !== 'function' || typeof sendError !== 'function' ||
        typeof engine === 'undefined' || typeof deviceInfo === 'undefined' ||
        typeof pixelBuf === 'undefined') {
      console.warn('sync-sim: simulator.js has changed, showing the preview without sync data');
      return;
    }
    const simulatorRender = render;
    self.render = () => {
      if (cfg.active && engine && deviceInfo.shape === 'circle') {
        syncRender();
      } else {
        simulatorRender();
      }
    };
  }

  // Page side.

  let cfg = loadConfig();
  let worker = null; // the simulator worker running now
  const RealWorker = window.Worker;

  function loadConfig() {
    let saved = {};
    try {
      saved = JSON.parse(localStorage.getItem(STORAGE_KEY)) || {};
    } catch (e) {
      // no storage: defaults
    }
    return normalize(Object.assign({}, DEFAULTS, saved, { overrides: {} }));
  }

  function saveConfig() {
    const { overrides, ...rest } = cfg;
    try {
      localStorage.setItem(STORAGE_KEY, JSON.stringify(rest));
    } catch (e) {
      // not saved, no harm
    }
  }

  function normalize(c) {
    const int = (x, min, max, fallback) => {
      x = Math.round(Number(x));
      return Number.isFinite(x) ? Math.min(Math.max(x, min), max) : fallback;
    };
    for (const key of ['active', 'locked', 'silent', 'swapRB', 'breakdowns']) {
      c[key] = !!c[key];
    }
    c.bpm = int(c.bpm, 30, 300, DEFAULTS.bpm);
    c.bars = int(c.bars, 1, 64, DEFAULTS.bars);
    c.pattern = c.pattern === 'auto' ? 'auto' : int(c.pattern, 0, 3, 'auto');
    for (const name in c.overrides) {
      const value = int(c.overrides[name], 0, 0xffff_ffff, null);
      if (value === null) {
        delete c.overrides[name];
      } else {
        c.overrides[name] = value;
      }
    }
    return c;
  }

  function update() {
    normalize(cfg);
    saveConfig();
    if (worker) {
      worker.postMessage({ type: 'syncsim', cfg });
    }
    refreshControls();
  }

  function isSimulator(url) {
    return /(^|\/)simulator\.js$/.test(new URL(url, document.baseURI).pathname);
  }

  function simulatorBlob(url) {
    const simulatorURL = new URL(url, document.baseURI).href;
    const args = [simulatorURL, cfg, PALETTES].map(a => JSON.stringify(a)).join(', ');
    const source = `(${workerMain})(${args});\n`;
    return URL.createObjectURL(new Blob([source], { type: 'text/javascript' }));
  }

  class SyncSimWorker extends RealWorker {
    constructor(url, options) {
      const blobURL = isSimulator(url) ? simulatorBlob(url) : null;
      super(blobURL || url, options);
      if (blobURL) {
        worker = this;
        this.addEventListener('message', () => URL.revokeObjectURL(blobURL), { once: true });
        this.addEventListener('message', (e) => {
          if (e.data && e.data.sync) {
            showStatus(e.data.sync);
          }
        });
      }
    }
  }

  // The device menu's "36-LED earring" restarts the preview (and makes sure
  // it is the earring's), without touching the code.
  function restartPreview() {
    const item = document.getElementById('dropdown-device-earringring36');
    if (item) {
      item.click();
    } else {
      console.warn('sync-sim: edit the code once to restart the preview');
    }
  }

  // The panel.

  const SLIDERS = ['level', 'bass', 'mid', 'high'];
  for (let i = 0; i < 8; i++) {
    SLIDERS.push(`bands[${i}]`);
  }
  SLIDERS.push('beat_phase');
  const SWITCHES = [
    ['active', 'Active'], ['locked', 'Locked'], ['silent', 'Silent'],
    ['swapRB', 'Swap R/B'], ['breakdowns', 'Breakdowns'],
  ];

  const panel = document.createElement('div');
  panel.className = 'card shadow small';
  panel.style.cssText = 'position: fixed; right: 1rem; bottom: 1rem; width: 20rem; ' +
    'max-width: calc(100vw - 2rem); max-height: calc(100vh - 2rem); overflow-y: auto; z-index: 1080;';
  panel.innerHTML = `
    <div class="card-header d-flex align-items-center py-1 px-2">
      <strong class="me-auto">Party sync data</strong>
      <button type="button" class="btn btn-sm py-0" data-act="fold" title="Fold">–</button>
      <button type="button" class="btn btn-sm py-0" data-act="uninstall" title="Stop faking sync data">✕</button>
    </div>
    <div class="card-body p-2" data-part="body">
      <div class="d-flex flex-wrap column-gap-3">
        ${SWITCHES.map(([key, label]) => `
          <div class="form-check form-switch mb-0">
            <input class="form-check-input" type="checkbox" role="switch" id="sync-sim-${key}" data-cfg="${key}">
            <label class="form-check-label" for="sync-sim-${key}">${label}</label>
          </div>`).join('')}
      </div>
      <div class="d-flex gap-2 align-items-center my-2">
        <label class="text-nowrap">BPM <input type="number" class="form-control form-control-sm d-inline-block" style="width: 4.5rem" min="30" max="300" data-cfg="bpm"></label>
        <label class="text-nowrap">Bars <input type="number" class="form-control form-control-sm d-inline-block" style="width: 3.5rem" min="1" max="64" data-cfg="bars"></label>
        <select class="form-select form-select-sm" style="width: auto" data-cfg="pattern" title="Pattern (color scheme)">
          <option value="auto">auto</option>
          <option value="0">0</option><option value="1">1</option>
          <option value="2">2</option><option value="3">3</option>
        </select>
      </div>
      <div class="d-flex gap-2 align-items-center">
        <button type="button" class="btn btn-sm btn-outline-secondary py-0 text-nowrap" data-act="section">Next section</button>
        <span class="text-body-secondary" data-part="status">waiting for the preview…</span>
      </div>
      <div class="progress my-1" style="height: 4px" title="beat_phase">
        <div class="progress-bar" style="transition: none" data-part="phase"></div>
      </div>
      <div class="d-flex mb-2" style="height: 8px" title="palette" data-part="palette"></div>
      <div style="display: grid; grid-template-columns: 5rem 1fr 2.2rem 1.4rem; align-items: center; column-gap: .4rem">
        ${SLIDERS.map(name => `
          <span>${name}</span>
          <input type="range" class="form-range" min="0" max="255" data-field="${name}">
          <output class="text-end" data-value="${name}">0</output>
          <button type="button" class="btn btn-sm p-0 border-0" style="visibility: hidden" data-release="${name}" title="Follow the music again">↺</button>`).join('')}
      </div>
    </div>`;

  const $ = selector => panel.querySelector(selector);
  const fieldInputs = {};
  for (const input of panel.querySelectorAll('[data-field]')) {
    fieldInputs[input.dataset.field] = input;
  }
  let shownPattern = null;

  panel.addEventListener('change', (e) => {
    const key = e.target.dataset.cfg;
    if (!key) {
      return;
    }
    cfg[key] = e.target.type === 'checkbox' ? e.target.checked : e.target.value;
    update();
  });
  panel.addEventListener('input', (e) => {
    const name = e.target.dataset.field;
    if (name) {
      cfg.overrides[name] = Number(e.target.value);
      update();
    }
  });
  panel.addEventListener('click', (e) => {
    const button = e.target.closest('button');
    if (!button) {
      return;
    }
    if (button.dataset.release) {
      window.syncSim.release(button.dataset.release);
    } else if (button.dataset.act === 'section') {
      window.syncSim.nextSection();
    } else if (button.dataset.act === 'fold') {
      $('[data-part=body]').classList.toggle('d-none');
    } else if (button.dataset.act === 'uninstall') {
      window.syncSim.uninstall();
    }
  });

  function refreshControls() {
    for (const input of panel.querySelectorAll('[data-cfg]')) {
      const value = cfg[input.dataset.cfg];
      if (input.type === 'checkbox') {
        input.checked = value;
      } else {
        input.value = value;
      }
    }
    for (const name of SLIDERS) {
      const held = name in cfg.overrides;
      $(`[data-release="${name}"]`).style.visibility = held ? 'visible' : 'hidden';
      if (held) {
        fieldInputs[name].value = cfg.overrides[name];
        $(`[data-value="${name}"]`).textContent = cfg.overrides[name];
      }
    }
  }

  function showStatus(v) {
    window.syncSim.status = v;
    if ($('[data-part=body]').classList.contains('d-none')) {
      return;
    }
    const drop = !(cfg.breakdowns && (v.sections & 1)) ? 'drop' : 'breakdown';
    $('[data-part=status]').textContent = `beat ${v.beats} · section ${v.sections} ` +
      `(${cfg.silent ? 'silent' : drop}) · hash ${v.hash.toString(16).padStart(8, '0')}`;
    $('[data-part=phase]').style.width = (v.beat_phase / 255 * 100) + '%';
    if (v.pattern !== shownPattern) {
      shownPattern = v.pattern;
      const swatch = $('[data-part=palette]');
      swatch.replaceChildren();
      for (let i = 0; i < 16; i++) {
        const cell = document.createElement('div');
        cell.style.flex = '1';
        cell.style.background = '#' + PALETTES.substr((v.pattern * 16 + i) * 6, 6);
        swatch.append(cell);
      }
      swatch.title = `palette of pattern ${v.pattern}`;
    }
    for (const name of SLIDERS) {
      if (!(name in cfg.overrides)) {
        fieldInputs[name].value = v[name];
        $(`[data-value="${name}"]`).textContent = v[name];
      }
    }
  }

  window.syncSim = {
    status: null,
    get config() {
      return JSON.parse(JSON.stringify(cfg));
    },
    set(changes) {
      Object.assign(cfg, changes, { overrides: cfg.overrides });
      update();
    },
    override(fields) {
      Object.assign(cfg.overrides, fields);
      update();
    },
    release(name) {
      if (name === undefined) {
        cfg.overrides = {};
      } else {
        delete cfg.overrides[name];
      }
      update();
    },
    nextSection() {
      if (worker) {
        worker.postMessage({ type: 'syncsim', cmd: 'section' });
      }
    },
    show() {
      if (!panel.isConnected) {
        document.body.append(panel);
      }
      $('[data-part=body]').classList.remove('d-none');
    },
    uninstall() {
      window.Worker = RealWorker;
      worker = null;
      panel.remove();
      delete window.syncSim;
      restartPreview();
    },
  };

  window.Worker = SyncSimWorker;
  refreshControls();
  window.syncSim.show();
  restartPreview();
})();
