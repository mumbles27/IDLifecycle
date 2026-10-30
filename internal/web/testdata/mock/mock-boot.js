// mock-boot.js - loads the in-memory mock API, optionally signs in, then starts the app or the selftest.
//   mock.html?autologin=admin#/approvals          -> app, already signed in as that mock user
//   mock-selftest.html?user=admin&pass=admin-pass -> runs ../selftest/selftest.js against the mock API
//   mock.html?live=1&autologin=admin#/approvals   -> as above, with dry run off (undo is refused in dry run)
import { db } from './mock-api.js';
import { login } from '../api.js';

const params = new URLSearchParams(location.search);
if (params.get('live') === '1') {
  db.settings.engine.dry_run = false;
}
const auto = params.get('autologin');
if (auto) {
  const u = db.users.find((x) => x.username === auto);
  await login(u.username, u.password);
}
if (document.getElementById('result')) {
  await import('../selftest/selftest.js');
}
else {
  await import('../app.js');
}

// mock.html?autologin=admin&click=[data-test=new-mapping]|[data-test=row-toggle]#/mappings
// Clicks the listed selectors in order (waiting for each to appear) - handy for screenshots.
const clicks = (params.get('click') || '').split('|').filter(Boolean);
for (const sel of clicks) {
  for (let i = 0; i < 200 && !document.querySelector(sel); i += 1) {
    await new Promise((r) => setTimeout(r, 50));
  }
  const el = document.querySelector(sel);
  if (el) {
    el.click();
    await new Promise((r) => setTimeout(r, 600));
  }
}
