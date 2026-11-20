// The pinned CLI's complete HTTP boundary. Every unlisted request fails offline.
import assert from 'node:assert/strict';
import crypto from 'node:crypto';
import fs from 'node:fs';
export const shouldUseTlsTransport = () => true;
export const browserIdentityForTlsProfile = () => ({userAgent: 'offline-test', acceptLanguage: 'en-US'});
let state;
let challenge;
const jwt = claims => 'e30.' + Buffer.from(JSON.stringify(claims)).toString('base64url') + '.synthetic';
export class TlsFingerprintTransport {
  enabled = true;
  async configure(proxy) {
    if (process.env.CODEX_TEST_MODE !== 'operation-error') {
      assert.equal(proxy, 'http://fixture-session:fixture@proxy.example.test:3128/');
    }
  }
  async request(method, url) {
    assert.equal(method, 'GET');
    assert.equal(url, 'https://auth.openai.com/');
    fs.appendFileSync(process.env.CODEX_TEST_ROUTES, 'PROBE auth.openai.com\n');
    return new Response('', {status: 302, headers: {location: '/log-in'}});
  }
  async close() {}
  async generateSentinelTokens() { return {token: 'synthetic-sentinel'}; }
  async fetch(url, options) {
    const target = new URL(url);
    assert.equal(target.origin, 'https://auth.openai.com');
    const route = `${options.method} ${target.pathname}`;
    fs.appendFileSync(process.env.CODEX_TEST_ROUTES, route + '\n');
    const mode = process.env.CODEX_TEST_MODE;
    const html = text => new Response(text || '<html>Offline</html>', {headers: {'content-type': 'text/html'}});
    const callback = () => 'http://localhost:1455/auth/callback?' + new URLSearchParams({
      ...(mode === 'missing-callback' ? {} : {code: 'synthetic-code'}),
      state: mode === 'wrong-state' ? 'wrong-state' : state,
    });
    if (route === 'POST /api/accounts/authorize/continue') {
      assert.equal(JSON.parse(options.body).username.kind, 'email');
      return Response.json({continue_url: mode === 'email-challenge'
        ? 'https://auth.openai.com/email-verification' : 'https://auth.openai.com/log-in/password'});
    }
    if (route === 'GET /log-in/password' || route === 'GET /log-in') return html();
    if (route === 'POST /api/accounts/password/verify') {
      assert.equal(JSON.parse(options.body).password, 'synthetic-password');
      if (mode === 'operation-error') return Response.json(JSON.parse(process.env.CODEX_TEST_ERROR), {status: 400});
      return Response.json({page: {type: 'mfa_challenge'}, 'oai-client-auth-session': {
        mfa_factors: [{factor_type: 'totp', id: 'synthetic-factor'}],
      }});
    }
    if (route === 'POST /api/accounts/mfa/issue_challenge') return Response.json({});
    if (route === 'POST /api/accounts/mfa/verify') {
      assert.match(JSON.parse(options.body).code, /^\d{6}$/);
      if (mode === 'session-rejected') return Response.json({continue_url: 'https://auth.openai.com/log-in'});
      if (mode.startsWith('workspace-')) return Response.json({
        'oai-client-auth-session': {workspaces: [{id: mode === 'workspace-success' ? 'expected-workspace' : 'other-workspace', kind: 'personal'}]},
      });
      return Response.json({continue_url: callback()});
    }
    if (route === 'GET /oauth/authorize') {
      state = target.searchParams.get('state');
      challenge = target.searchParams.get('code_challenge');
      assert.ok(state);
      assert.equal(target.searchParams.get('code_challenge_method'), 'S256');
      assert.ok(challenge);
      assert.equal(target.searchParams.get('client_id'), 'app_EMoamEEZ73f0CkXaXp7hrann');
      assert.equal(target.searchParams.get('redirect_uri'), 'http://localhost:1455/auth/callback');
      assert.equal(target.searchParams.get('scope'), 'openid profile email offline_access');
      return new Response('', {status: 302, headers: {location: '/log-in'}});
    }
    if (route === 'POST /api/accounts/workspace/select') {
      assert.equal(JSON.parse(options.body).workspace_id, 'expected-workspace');
      return Response.json({continue_url: callback()});
    }
    if (route === 'POST /oauth/token') {
      const form = new URLSearchParams(options.body);
      assert.equal(form.get('code'), 'synthetic-code');
      assert.match(form.get('code_verifier'), /^[A-Za-z0-9._~-]{43,128}$/);
      assert.equal(crypto.createHash('sha256').update(form.get('code_verifier')).digest('base64url'), challenge);
      if (mode === 'exchange-failed') return Response.json({error: 'synthetic-private'}, {status: 400});
      return Response.json({access_token: 'synthetic-access',
        ...(mode === 'incomplete' ? {} : {refresh_token: 'synthetic-refresh'}),
        id_token: jwt({email: 'test@example.test', sid: 'synthetic-account', exp: 4000000000,
          'https://api.openai.com/auth': {user_id: 'synthetic-user'}})});
    }
    throw new Error('UNEXPECTED_OFFLINE_HTTP_REQUEST');
  }
}
