import test from 'node:test';
import assert from 'node:assert/strict';
globalThis.localStorage = { getItem: () => null, setItem() {}, removeItem() {} };
const { verdict, heat } = await import('../../internal/api/dashboard/screens/devices.js');

const GB = 2 ** 30;
const card = { name: 'NVIDIA GeForce RTX 3060', percent: 1, memory_used: 0.4 * GB, memory_total: 12 * GB };
const pc = (o = {}) => ({
  cpu: { percent: 3 }, memory: { total: 16 * GB, available: 9 * GB }, gpus: [card],
  groups: [{ name: 'Windows and other', cpu_percent: 2 }, { name: 'WhisperX transcription', cpu_percent: 1, gpu: true }], ...o,
});

test('the verdict names what is wrong, first rule first', () => {
  assert.deepEqual(verdict(pc()), { cls: 'ok', short: 'Free for heavy work', text: 'Free for heavy work: 97% of the processor and 9 GB of memory are spare.' });
  assert.equal(verdict(pc({ memory: { total: 16 * GB, available: 0.8 * GB }, cpu: { percent: 95 } })).short, 'Low on memory');
  assert.equal(verdict(pc({ gpus: [{ ...card, memory_used: 11.5 * GB }] })).text, 'Busy: WhisperX transcription is using the graphics card.');
  assert.equal(verdict(pc({ gpus: [{ ...card, percent: 90 }], groups: [] })).text, 'Busy: the graphics card is in use.');
  assert.equal(verdict(pc({ cpu: { percent: 85 } })).text, 'Busy: 85% of the processor is in use, mostly by Windows and other.');
  assert.equal(verdict(pc({ cpu: { percent: 60 } })).short, 'Partly busy');
  assert.equal(verdict(pc({ memory: { total: 16 * GB, available: 3 * GB } })).text, 'Light work only: 3 GB of memory is free.');
  // A machine on an older proxy has no groups and no card.
  assert.equal(verdict({ cpu: { percent: 90 }, memory: { total: 8 * GB, available: 4 * GB }, top_cpu: [{ name: 'node' }] }).text, 'Busy: 90% of the processor is in use, mostly by node.');
});

test('temperatures in plain words, cards hotter sooner than processors', () => {
  const st = pc();
  assert.equal(heat({ label: card.name, celsius: 45 }, st).word, 'normal');
  assert.equal(heat({ label: card.name, celsius: 82 }, st).word, 'warm');
  assert.equal(heat({ label: card.name, celsius: 90 }, st).word, 'hot');
  assert.equal(heat({ label: 'CPU', celsius: 90 }, st).word, 'warm');
  assert.equal(heat({ label: 'CPU', celsius: 90 }, st).part, 'processor');
});
