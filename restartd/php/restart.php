<?php
declare(strict_types=1);

/*
 * Bridge service control - a self-contained page for restartd.
 *
 * Put this file and restart-config.php (copy restart-config.sample.php) in one
 * directory on the Apache2 server, ideally outside anything else public. It
 * talks straight to restartd on the bridge server; no relay-admin, no nginx
 * vhost. See restartd/README.md for setup and the security notes.
 *
 * Layers: HTTPS only -> optional Apache client-cert / IP allowlist -> login
 * (password_hash) with brute-force lockout -> CSRF token on every action ->
 * every call to restartd is HMAC-signed (secret never leaves the config file).
 */

$configFile = __DIR__ . '/restart-config.php';
if (!is_file($configFile)) {
    http_response_code(500);
    exit('restart-config.php is missing (copy restart-config.sample.php and fill it in).');
}
$config = require $configFile;

// ---- transport / network gates ---------------------------------------------
$https = !empty($_SERVER['HTTPS']) && strtolower((string)$_SERVER['HTTPS']) !== 'off';
if (!$https && empty($config['allow_http'])) {
    http_response_code(403);
    exit('HTTPS required.');
}
if (!empty($config['allowed_ips']) && !in_array($_SERVER['REMOTE_ADDR'] ?? '', $config['allowed_ips'], true)) {
    http_response_code(403);
    exit('Forbidden.');
}
if (!empty($config['require_client_cert']) && (($_SERVER['SSL_CLIENT_VERIFY'] ?? '') !== 'SUCCESS')) {
    http_response_code(403);
    exit('A valid client certificate is required.');
}

// ---- session ------------------------------------------------------------------
session_name($config['session_name'] ?? 'rdctl');
session_set_cookie_params([
    'lifetime' => 0,
    'path' => '/',
    'secure' => $https,
    'httponly' => true,
    'samesite' => 'Strict',
]);
session_start();
$lifetime = (int)($config['session_lifetime'] ?? 3600);
if (isset($_SESSION['last']) && time() - $_SESSION['last'] > $lifetime) {
    $_SESSION = [];
}
$_SESSION['last'] = time();
if (empty($_SESSION['csrf'])) {
    $_SESSION['csrf'] = bin2hex(random_bytes(32));
}

$nonce = base64_encode(random_bytes(16));
header('Cache-Control: no-store');
header('X-Frame-Options: DENY');
header('X-Content-Type-Options: nosniff');
header('Referrer-Policy: no-referrer');
header("Content-Security-Policy: default-src 'none'; style-src 'nonce-$nonce'; script-src 'nonce-$nonce'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'");

function h(string $s): string
{
    return htmlspecialchars($s, ENT_QUOTES | ENT_SUBSTITUTE, 'UTF-8');
}

// ---- login throttle (per client address, kept in the temp dir) ----------------
function throttle_file(): string
{
    return sys_get_temp_dir() . '/rdctl-login-' . hash('sha256', ($_SERVER['REMOTE_ADDR'] ?? '?')) . '.json';
}
function throttle_blocked(): bool
{
    $d = @json_decode((string)@file_get_contents(throttle_file()), true) ?: [];
    return ($d['until'] ?? 0) > time();
}
function throttle_fail(): void
{
    $f = throttle_file();
    $d = @json_decode((string)@file_get_contents($f), true) ?: [];
    $d['fails'] = array_values(array_filter($d['fails'] ?? [], fn($t) => $t > time() - 900));
    $d['fails'][] = time();
    if (count($d['fails']) >= 5) {
        $d['until'] = time() + 900;
        $d['fails'] = [];
    }
    @file_put_contents($f, json_encode($d), LOCK_EX);
}
function throttle_clear(): void
{
    @unlink(throttle_file());
}

// ---- signed calls to restartd -------------------------------------------------
function rd_call(array $config, string $method, string $path, ?array $payload = null): array
{
    $body = $payload === null ? '' : json_encode($payload, JSON_UNESCAPED_SLASHES);
    $ts = (string)time();
    $nonce = bin2hex(random_bytes(12));
    $msg = implode("\n", ['v1', $method, $path, $ts, $nonce, hash('sha256', $body)]);
    $sig = hash_hmac('sha256', $msg, (string)$config['secret']);
    $ch = curl_init(rtrim((string)$config['backend'], '/') . $path);
    curl_setopt_array($ch, [
        CURLOPT_RETURNTRANSFER => true,
        CURLOPT_CUSTOMREQUEST => $method,
        CURLOPT_CONNECTTIMEOUT => (int)($config['timeout'] ?? 6),
        CURLOPT_TIMEOUT => (int)($config['timeout'] ?? 6),
        CURLOPT_FOLLOWLOCATION => false,
        CURLOPT_HTTPHEADER => [
            'Content-Type: application/json',
            "X-RD-Timestamp: $ts",
            "X-RD-Nonce: $nonce",
            "X-RD-Signature: $sig",
        ],
    ]);
    if ($method === 'POST') {
        curl_setopt($ch, CURLOPT_POSTFIELDS, $body);
    }
    $out = curl_exec($ch);
    $code = (int)curl_getinfo($ch, CURLINFO_HTTP_CODE);
    $err = curl_error($ch);
    curl_close($ch);
    if ($out === false) {
        return ['http' => 0, 'ok' => false, 'error' => 'cannot reach restartd (' . $err . ')'];
    }
    $data = json_decode($out, true);
    if (!is_array($data)) {
        return ['http' => $code, 'ok' => false, 'error' => 'unexpected answer from restartd'];
    }
    $data['http'] = $code;
    return $data;
}

function json_out(array $data, int $code = 200): void
{
    http_response_code($code);
    header('Content-Type: application/json');
    echo json_encode($data);
    exit;
}

// ---- actions ------------------------------------------------------------------
$authed = !empty($_SESSION['authed']);
$action = (string)($_GET['action'] ?? $_POST['action'] ?? '');
$flash = '';
$flashOk = true;

if ($_SERVER['REQUEST_METHOD'] === 'POST') {
    $token = (string)($_POST['csrf'] ?? '');
    if (!hash_equals($_SESSION['csrf'], $token)) {
        http_response_code(403);
        exit('Bad CSRF token - reload the page.');
    }
    if ($action === 'login') {
        if (throttle_blocked()) {
            $flash = 'Too many failed attempts. Try again in a few minutes.';
            $flashOk = false;
        } elseif (password_verify((string)($_POST['password'] ?? ''), (string)$config['password_hash'])) {
            session_regenerate_id(true);
            $_SESSION['authed'] = true;
            $_SESSION['last'] = time();
            $_SESSION['csrf'] = bin2hex(random_bytes(32));
            throttle_clear();
            header('Location: ' . strtok($_SERVER['REQUEST_URI'], '?'));
            exit;
        } else {
            throttle_fail();
            usleep(400000);
            $flash = 'Wrong password.';
            $flashOk = false;
        }
    } elseif ($action === 'logout') {
        $_SESSION = [];
        session_destroy();
        header('Location: ' . strtok($_SERVER['REQUEST_URI'], '?'));
        exit;
    } elseif ($authed && $action === 'restart') {
        $svc = (string)($_POST['service'] ?? '');
        $res = rd_call($config, 'POST', '/v1/restart', [
            'service' => $svc,
            'rebuild' => ($_POST['rebuild'] ?? '0') === '1',
        ]);
        if (($_POST['ajax'] ?? '') === '1') {
            json_out(['ok' => !empty($res['ok']), 'error' => $res['error'] ?? null, 'service' => $svc, 'rebuild' => ($_POST['rebuild'] ?? '0') === '1']);
        }
        $flashOk = !empty($res['ok']);
        $flash = $flashOk ? "Restart of $svc queued." : ('Failed: ' . ($res['error'] ?? 'unknown error'));
    } else {
        http_response_code(403);
        exit('Not allowed.');
    }
}

if ($action === 'status' && $_SERVER['REQUEST_METHOD'] === 'GET') {
    if (!$authed) {
        json_out(['ok' => false, 'error' => 'not signed in'], 401);
    }
    $res = rd_call($config, 'GET', '/v1/services');
    json_out($res, empty($res['ok']) ? 502 : 200);
}
?>
<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex,nofollow">
<title>Bridge control</title>
<style nonce="<?= h($nonce) ?>">
:root{--bg:#0f1218;--card:#181d27;--line:#2a3140;--txt:#e6e9ef;--dim:#8b93a7;--ok:#3fb950;--warn:#d29922;--bad:#f85149;--acc:#58a6ff}
@media (prefers-color-scheme: light){:root{--bg:#f4f6fa;--card:#fff;--line:#d9dee8;--txt:#1b2130;--dim:#5d667a;--acc:#0b63ce}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--txt);font:14px/1.45 system-ui,sans-serif;padding:16px}
main{max-width:1200px;margin:0 auto}h1{font-size:20px;margin:0 0 4px}.sub{color:var(--dim);margin:0 0 14px}
.card{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:14px;margin-bottom:14px}
.host{display:flex;flex-wrap:wrap;gap:8px 22px}.host div{min-width:120px}.k{color:var(--dim);font-size:12px}.v{font-size:16px;font-weight:600}
table{width:100%;border-collapse:collapse}th,td{padding:7px 8px;border-bottom:1px solid var(--line);text-align:right;white-space:nowrap}
th{color:var(--dim);font-weight:500;font-size:12px}th:first-child,td:first-child,th:nth-child(2),td:nth-child(2){text-align:left}
.wrap{overflow-x:auto}.pill{display:inline-block;padding:1px 8px;border-radius:99px;font-size:12px;border:1px solid var(--line)}
.running{color:var(--ok);border-color:var(--ok)}.down,.build_failed{color:var(--bad);border-color:var(--bad)}.building,.restarting{color:var(--warn);border-color:var(--warn)}
button{font:inherit;padding:4px 10px;border-radius:7px;border:1px solid var(--line);background:transparent;color:var(--txt);cursor:pointer}
button:hover{border-color:var(--acc);color:var(--acc)}button:disabled{opacity:.4;cursor:default}
input[type=password]{font:inherit;padding:8px 10px;border-radius:7px;border:1px solid var(--line);background:var(--bg);color:var(--txt);width:100%}
.flash{padding:9px 12px;border-radius:8px;margin-bottom:12px;border:1px solid var(--line)}.flash.ok{border-color:var(--ok)}.flash.bad{border-color:var(--bad)}
.msg{color:var(--dim);font-size:12px;white-space:normal;max-width:260px;text-align:left}.note{color:var(--dim);font-size:12px;margin-top:8px}
.top{display:flex;justify-content:space-between;align-items:flex-start;gap:12px}
</style>
</head>
<body>
<main>
<?php if (!$authed): ?>
  <div class="card" style="max-width:380px;margin:12vh auto 0">
    <h1>Bridge control</h1>
    <p class="sub">Sign in to continue.</p>
    <?php if ($flash !== ''): ?><div class="flash <?= $flashOk ? 'ok' : 'bad' ?>"><?= h($flash) ?></div><?php endif; ?>
    <form method="post" autocomplete="off">
      <input type="hidden" name="action" value="login">
      <input type="hidden" name="csrf" value="<?= h($_SESSION['csrf']) ?>">
      <p><input type="password" name="password" placeholder="Password" autofocus required></p>
      <button type="submit">Sign in</button>
    </form>
  </div>
<?php else: ?>
  <div class="top">
    <div><h1>Bridge control</h1><p class="sub" id="updated">loading...</p></div>
    <form method="post"><input type="hidden" name="action" value="logout"><input type="hidden" name="csrf" value="<?= h($_SESSION['csrf']) ?>"><button type="submit">Sign out</button></form>
  </div>
  <div id="flash" class="flash <?= $flash === '' ? '' : ($flashOk ? 'ok' : 'bad') ?>" <?= $flash === '' ? 'hidden' : '' ?>><?= h($flash) ?></div>
  <div class="card host" id="host"></div>
  <div class="card wrap">
    <table>
      <thead><tr>
        <th>Service</th><th>State</th><th>PID</th><th>CPU</th><th>Memory</th>
        <th>Disk read</th><th>Disk write</th><th>TCP conns</th><th>TCP in</th><th>TCP out</th><th>UDP socks</th><th></th>
      </tr></thead>
      <tbody id="rows"><tr><td colspan="12">loading...</td></tr></tbody>
    </table>
    <p class="note">CPU is per core (200% = two cores busy). Disk, memory and processes include each service's child processes. TCP rates come from live connections. Linux keeps no per-process UDP counters, so game (UDP) traffic shows as socket counts here and as host-wide network totals above.
    &ldquo;Restart&rdquo; stops and starts the service; &ldquo;Rebuild&rdquo; compiles it first and leaves the running copy alone if the build fails.</p>
  </div>
  <script nonce="<?= h($nonce) ?>">
  const CSRF = <?= json_encode($_SESSION['csrf']) ?>;
  const $ = (id) => document.getElementById(id);
  const esc = (s) => String(s).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  const bytes = (n) => { if (n == null) return '-'; const u = ['B','KB','MB','GB','TB']; let i = 0; while (n >= 1024 && i < 4) { n /= 1024; i++; } return (i ? n.toFixed(n < 10 ? 1 : 0) : n) + ' ' + u[i]; };
  const rate = (n) => n == null ? '-' : bytes(n) + '/s';
  const pct = (n) => n == null ? '-' : n.toFixed(1) + '%';
  function flash(text, ok) { const f = $('flash'); f.hidden = false; f.className = 'flash ' + (ok ? 'ok' : 'bad'); f.textContent = text; }
  async function act(name, rebuild) {
    if (!confirm((rebuild ? 'Rebuild and restart ' : 'Restart ') + name + '?')) return;
    const body = new URLSearchParams({action: 'restart', csrf: CSRF, service: name, rebuild: rebuild ? '1' : '0', ajax: '1'});
    try {
      const r = await fetch(location.pathname, {method: 'POST', body, credentials: 'same-origin'});
      const j = await r.json();
      flash(j.ok ? (rebuild ? 'Rebuild + restart of ' : 'Restart of ') + name + ' queued.' : 'Failed: ' + (j.error || 'unknown error'), j.ok);
    } catch (e) { flash('Request failed: ' + e, false); }
    setTimeout(refresh, 1500);
  }
  function render(d) {
    const h = d.host || {};
    const memUsed = h.mem_total ? h.mem_total - h.mem_available : null;
    $('host').innerHTML = [
      ['Host CPU', pct(h.cpu_percent)],
      ['Memory', memUsed == null ? '-' : bytes(memUsed) + ' / ' + bytes(h.mem_total)],
      ['Load', h.load ? h.load.map(x => x.toFixed(2)).join(' ') : '-'],
      ['Network in', rate(h.net_rx_bps)],
      ['Network out', rate(h.net_tx_bps)],
    ].map(([k, v]) => '<div><div class="k">' + k + '</div><div class="v">' + esc(v) + '</div></div>').join('');
    $('rows').innerHTML = d.services.map(s => {
      const m = s.metrics || {}, st = (s.status && s.status.state) || (s.running ? 'running' : 'down');
      const msg = s.status && s.status.message ? '<div class="msg">' + esc(s.status.message) + '</div>' : '';
      const busy = s.pending || st === 'building' || st === 'restarting';
      return '<tr><td>' + esc(s.name) + msg + '</td><td><span class="pill ' + esc(st) + '">' + esc(s.pending ? 'queued' : st) + '</span></td>'
        + '<td>' + (s.pid || '-') + '</td><td>' + pct(m.cpu_percent) + '</td><td>' + bytes(m.rss_bytes) + '</td>'
        + '<td>' + rate(m.disk_read_bps) + '</td><td>' + rate(m.disk_write_bps) + '</td>'
        + '<td>' + (m.tcp_connections ?? '-') + '</td><td>' + rate(m.tcp_rx_bps) + '</td><td>' + rate(m.tcp_tx_bps) + '</td><td>' + (m.udp_sockets ?? '-') + '</td>'
        + '<td><button data-n="' + esc(s.name) + '" data-r="0"' + (busy ? ' disabled' : '') + '>Restart</button> '
        + (s.buildable ? '<button data-n="' + esc(s.name) + '" data-r="1"' + (busy ? ' disabled' : '') + '>Rebuild</button>' : '') + '</td></tr>';
    }).join('');
    $('updated').textContent = 'updated ' + new Date().toLocaleTimeString() + ' - refreshes every 4 s';
  }
  $('rows').addEventListener('click', (e) => { const b = e.target.closest('button'); if (b && !b.disabled) act(b.dataset.n, b.dataset.r === '1'); });
  async function refresh() {
    try {
      const r = await fetch(location.pathname + '?action=status', {credentials: 'same-origin', cache: 'no-store'});
      if (r.status === 401) { location.reload(); return; }
      const d = await r.json();
      if (!d.ok) { $('updated').textContent = 'restartd unreachable: ' + (d.error || r.status); return; }
      render(d);
    } catch (e) { $('updated').textContent = 'update failed: ' + e; }
  }
  refresh(); setInterval(refresh, 4000);
  </script>
<?php endif; ?>
</main>
</body>
</html>
