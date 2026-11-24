// SPDX-License-Identifier: Apache-2.0
// Sets data-theme on <html> before the first paint, so the page never flashes the wrong theme.
// The UI sends script-src 'self', so this is a file and not an inline script. The same rules live
// in src/lib/theme.ts; keep the storage key and the rules in step (src/components/theme-switch.test.tsx checks it).
;(function () {
  var choice = 'system'
  try {
    var saved = localStorage.getItem('koinon-theme')
    if (saved === 'dark' || saved === 'light' || saved === 'system') choice = saved
  } catch {}
  var light = choice === 'light' || (choice === 'system' && window.matchMedia('(prefers-color-scheme: light)').matches)
  document.documentElement.setAttribute('data-theme', light ? 'light' : 'dark')
})()
