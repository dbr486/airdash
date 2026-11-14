import test from 'node:test';
import assert from 'node:assert/strict';

await import('../public/jsonview.js');
const JV = globalThis.JsonView;

test('render marks each leaf type and shows child counts', () => {
  const html = JV.render({ a: 'x', n: 1, b: true, z: null, l: [1, 2] });
  for (const c of ['jv-str', 'jv-num', 'jv-bool', 'jv-null', 'jv-key']) assert.ok(html.includes(c), c);
  assert.match(html, />5</);
  assert.match(html, />2</);
});

test('render escapes values and keys', () => {
  const html = JV.render({ s: '<img src=x onerror=alert(1)>', '<b>': 1 });
  assert.ok(html.includes('&lt;img'));
  assert.ok(!html.includes('<img'));
  assert.ok(!html.includes('<b>'));
});

test('arrays over 50 items are lazy and expand on demand', () => {
  const root = { l: Array.from({ length: 51 }, (_, i) => i) };
  const html = JV.render(root);
  assert.ok(html.includes('jv-lazy'));
  assert.ok(!html.includes('jv-num'));
  const kids = JV.renderChildren(root, '/l');
  assert.equal(kids.split('jv-num').length - 1, 51);
});

test('search returns paths of matching keys and leaves, case-insensitive', () => {
  assert.deepEqual(JV.search({ a: { city: 'Malaga' }, b: ['MALAGA', 3] }, 'malaga'), ['/a/city', '/b/0']);
  assert.deepEqual(JV.search({ city: 1 }, 'CIT'), ['/city']);
});

test('paths escape ~ and / per RFC 6901 and renderChildren resolves them', () => {
  const root = { 'a/b': { 'c~d': Array.from({ length: 60 }, (_, i) => i) } };
  assert.deepEqual(JV.search(root, 'c~d'), ['/a~1b/c~0d']);
  const html = JV.render(root);
  assert.ok(html.includes('data-path="/a~1b/c~0d"'));
  assert.equal(JV.renderChildren(root, '/a~1b/c~0d').split('jv-num').length - 1, 60);
});

test('search ignores terms shorter than 2 characters', () => {
  assert.deepEqual(JV.search({ a: 'x', b: 'xy' }, 'x'), []);
  assert.deepEqual(JV.search({ a: 'x', b: 'xy' }, ' '), []);
  assert.deepEqual(JV.search({ a: 'x', b: 'xy' }, 'xy'), ['/b']);
});

test('search stops after the limit', () => {
  const big = Array.from({ length: 500 }, (_, i) => ({ flight: 'AB' + i }));
  assert.equal(JV.search(big, 'ab').length, 500);
  assert.equal(JV.search(big, 'ab', 201).length, 201);
});

test('ancestors start at the root so a collapsed or lazy root is opened first', () => {
  assert.deepEqual(JV.ancestors('/0/a~1b/c'), ['', '/0', '/0/a~1b']);
  assert.deepEqual(JV.ancestors('/x'), ['']);
  assert.deepEqual(JV.ancestors(''), []);
});

// Just enough of an element for mount() to wire its listener.
function fakeEl() {
  return {
    innerHTML: '',
    live: new Set(),
    addEventListener(type, fn) { if (type === 'toggle') this.live.add(fn); },
    removeEventListener(type, fn) { if (type === 'toggle') this.live.delete(fn); },
  };
}

test('remounting the same element keeps a single toggle listener', () => {
  const el = fakeEl();
  JV.mount(el, '{"a":[1]}');
  JV.mount(el, '{"b":[2]}');
  assert.equal(el.live.size, 1);
  JV.mount(el, '');
  assert.equal(el.live.size, 0);
});

test('find on a non-JSON body reports no matches in the result shape', () => {
  const el = fakeEl();
  assert.deepEqual(JV.mount(el, 'plain text').find('pl'), { count: 0, more: false });
});
