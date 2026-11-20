import assert from 'node:assert/strict';
import crypto from 'node:crypto';
import test from 'node:test';
import {loginCodexPassword, prepareCodexTransport, requireCodexRedirect} from './openai_codex_password_flow.mjs';

const authBase = 'https://auth.openai.com';
const redirectUri = 'http://localhost:1455/auth/callback';
const clientId = 'app_EMoamEEZ73f0CkXaXp7hrann';

function fixture(mode) {
  const trace = [];
  let authorize;
  const client = {
    jar: {value: () => 'fixture-device'},
    async follow(url) {
      const target = new URL(url);
      trace.push(target.pathname);
      if (target.pathname === '/oauth/authorize') authorize = target;
      return {finalUrl: target.pathname === '/oauth/authorize' ? `${authBase}/log-in` : url};
    },
  };
  const helpers = {
    isAuthLoginPage: value => value === `${authBase}/log-in`,
    isPasswordLoginPage: value => value === `${authBase}/log-in/password`,
    createSentinelHeaders: async () => ({'openai-sentinel-token': 'fixture'}),
    async authJsonStep(_client, origin, method, pathname, body) {
      assert.equal(origin, authBase);
      assert.equal(method, 'POST');
      trace.push(pathname);
      if (pathname === '/api/accounts/authorize/continue') {
        assert.deepEqual(body, {username: {kind: 'email', value: 'fixture@example.test'}});
        return {data: {continue_url: `${authBase}/${mode === 'email' ? 'email-verification' : 'log-in/password'}`}};
      }
      assert.equal(pathname, '/api/accounts/workspace/select');
      assert.equal(body.workspace_id, 'workspace-fixture');
      const state = mode === 'wrong-state' ? 'incorrect' : authorize.searchParams.get('state');
      const code = mode === 'missing-code' ? '' : '&code=fixture-code';
      return {data: {continue_url: `${redirectUri}?state=${state}${code}`}};
    },
    getContinueUrl: payload => payload?.continue_url,
    async verifyPassword(_client, options) {
      assert.equal(options.password, 'fixture-password');
      trace.push('password');
      return {page: {type: 'mfa_challenge'}};
    },
    isMfaChallengePayload: payload => payload?.page?.type === 'mfa_challenge',
    async completeTotpMfaIfNeeded(_client, options) {
      assert.equal(options.totpSecret, 'fixture-secret');
      trace.push('totp');
      return {'oai-client-auth-session': {workspaces: [{id: 'workspace-fixture'}]}};
    },
    pickWorkspaceId: payload => payload['oai-client-auth-session'].workspaces[0].id,
    continueFlow: async (_client, payload) => ({finalUrl: payload.continue_url}),
  };
  const options = {authBase, clientId, redirectUri, email: 'fixture@example.test', password: 'fixture-password', totpSecret: 'fixture-secret'};
  const diagnostic = {mark: stage => trace.push(stage)};
  return {client, start: () => loginCodexPassword(client, options, helpers, diagnostic), trace, query: () => authorize.searchParams};
}

test('Codex authenticates and selects the workspace inside one PKCE authorization', async () => {
  const {client, start, trace, query} = fixture('success');
  const login = await start();
  assert.equal(login.mfaVerified, true);
  assert.equal(login.callbackUrl, client.codexPasswordOAuth.callbackUrl);
  assert.equal(client.codexPasswordOAuth.workspaceId, 'workspace-fixture');
  assert.equal(query().get('client_id'), clientId);
  assert.equal(query().get('redirect_uri'), redirectUri);
  assert.equal(query().get('scope'), 'openid profile email offline_access');
  assert.equal(query().get('state'), client.codexPasswordOAuth.state);
  assert.equal(query().get('code_challenge'), crypto.createHash('sha256').update(client.codexPasswordOAuth.codeVerifier).digest('base64url'));
  assert.deepEqual(trace, ['oauth_authorize', '/oauth/authorize', '/api/accounts/authorize/continue', '/log-in/password', 'web_login',
    'password', 'totp', 'workspace_select', '/api/accounts/workspace/select', 'callback_validation']);
});

for (const [mode, error] of [['email', 'interaction_required'], ['wrong-state', 'callback_state_mismatch'], ['missing-code', 'callback_missing']]) {
  test(`Codex rejects ${mode} without publishing a callback`, async () => {
    const {client, start, trace} = fixture(mode);
    await assert.rejects(start, {message: error});
    assert.equal(client.codexPasswordOAuth, undefined);
    if (mode === 'email') assert.ok(!trace.includes('password'));
  });
}

test('Codex probes auth using the exact proxy and stops on a security challenge', async () => {
  const proxy = 'http://fixture-session-reuse:fixture@proxy.example.test:3128';
  const trace = [];
  const transport = {
    async configure(value, options) { trace.push([value, options]); },
    async request(method, url) {
      trace.push([method, url]);
      return new Response('', {status: 403, headers: {'cf-mitigated': 'challenge'}});
    },
  };
  await assert.rejects(prepareCodexTransport(transport, proxy, authBase, () => ({userAgent: 'fixture'})), {message: 'security_check'});
  assert.deepEqual(trace, [[proxy, {force: true}], ['GET', `${authBase}/`]]);
});

test('Codex only follows auth redirects and the exact loopback callback', () => {
  requireCodexRedirect(`${authBase}/log-in`, redirectUri);
  requireCodexRedirect(`${redirectUri}?state=fixture`, redirectUri);
  assert.throws(() => requireCodexRedirect('https://chatgpt.com/log-in', redirectUri), {message: 'callback_invalid'});
  assert.throws(() => requireCodexRedirect('http://localhost:1234/auth/callback', redirectUri), {message: 'callback_invalid'});
});
