<?php
// Copy to restart-config.php (same directory as restart.php) and fill it in.
// Keep this file out of any public/Git location: it holds the signing secret.
return [
    // Where restartd listens on the bridge server (start.sh starts it; default port 9333).
    'backend' => 'http://YOUR-BRIDGE-SERVER:9333',

    // The contents of restartd/secret.key on the bridge server (created on its first start).
    'secret' => 'PASTE-THE-64-HEX-CHARACTERS-FROM-secret.key',

    // Login password for this page. Generate the hash with:
    //   php -r 'echo password_hash("choose-a-long-password", PASSWORD_DEFAULT), "\n";'
    'password_hash' => 'PASTE-PASSWORD-HASH',

    // Optional extra gates (all default to off):
    'require_client_cert' => false, // require Apache to have verified a client cert (SSLVerifyClient require + SSLOptions +StdEnvVars)
    'allowed_ips' => [],            // e.g. ['203.0.113.7'] - empty allows any address
    'allow_http' => false,          // leave false: the login password would travel unencrypted otherwise

    'session_name' => 'rdctl',
    'session_lifetime' => 3600,     // idle seconds before you have to sign in again
    'timeout' => 6,                 // seconds to wait for restartd
];
