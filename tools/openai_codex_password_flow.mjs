import crypto from 'node:crypto';
import {validateCodexCallback} from './openai_codex_diagnostics.mjs';

const authOrigin = 'https://auth.openai.com';

export async function prepareCodexTransport(transport, proxy, authBase, browserIdentity) {
  if (authBase !== authOrigin) throw new Error('callback_invalid');
  try {
    await transport.configure(proxy || null, {force: true});
  } catch {
    throw new Error('transport_error');
  }
  if (!proxy) return;

  let response;
  try {
    response = await transport.request('GET', `${authBase}/`, {
      headers: {'user-agent': browserIdentity(transport.identityProfile).userAgent},
    });
  } catch {
    throw new Error('transport_error');
  }
  if (response.headers.get('cf-mitigated') || response.headers.get('x-cf-mitigated')) {
    throw new Error('security_check');
  }
  if (response.status === 429) throw new Error('rate_limited');
  if (response.status >= 500) throw new Error('service_error');
  if (response.status < 200 || response.status >= 400) throw new Error('authorization_denied');
}

export function requireCodexRedirect(url, redirectUri) {
  let target;
  try { target = new URL(url); }
  catch { throw new Error('callback_invalid'); }
  const callback = new URL(redirectUri);
  if (target.username || target.password || (target.origin !== authOrigin &&
      (target.origin !== callback.origin || target.pathname !== callback.pathname))) {
    throw new Error('callback_invalid');
  }
}

function rejectAdditionalVerification(url, payload) {
  const kind = payload?.page?.type;
  const pathname = url ? new URL(url).pathname : '';
  if (kind === 'add_phone' || ['/add-phone', '/phone-verification'].includes(pathname)) {
    throw new Error('phone_verification_required');
  }
  if (kind === 'email_verification' || kind === 'profile' || ['/email-verification', '/about-you'].includes(pathname)) {
    throw new Error('interaction_required');
  }
}

export async function loginCodexPassword(client, options, helpers, diagnostic) {
  const {authBase, email, password, totpSecret, rl, clientId, redirectUri} = options;
  if (authBase !== authOrigin || !email || !password) throw new Error('interaction_required');
  const codeVerifier = crypto.randomBytes(48).toString('base64url');
  const state = crypto.randomBytes(24).toString('base64url');
  const query = new URLSearchParams({
    client_id: clientId,
    code_challenge: crypto.createHash('sha256').update(codeVerifier).digest('base64url'),
    code_challenge_method: 'S256',
    codex_cli_simplified_flow: 'true',
    id_token_add_organizations: 'true',
    redirect_uri: redirectUri,
    response_type: 'code',
    scope: 'openid profile email offline_access',
    state,
  });

  diagnostic.mark('oauth_authorize');
  let page = await client.follow(`${authBase}/oauth/authorize?${query}`);
  rejectAdditionalVerification(page.finalUrl);
  const deviceId = client.jar.value('oai-did', authBase) || crypto.randomUUID();
  if (!helpers.isPasswordLoginPage(page.finalUrl)) {
    if (!helpers.isAuthLoginPage(page.finalUrl)) throw new Error('session_unavailable');
    const headers = await helpers.createSentinelHeaders(client, {authBase, deviceId, flow: 'authorize_continue'});
    const {data} = await helpers.authJsonStep(client, authBase, 'POST', '/api/accounts/authorize/continue',
      {username: {kind: 'email', value: email}}, {referer: page.finalUrl, headers});
    const next = helpers.getContinueUrl(data);
    rejectAdditionalVerification(next, data);
    if (!next || !helpers.isPasswordLoginPage(next)) throw new Error('interaction_required');
    page = await client.follow(next, {referer: page.finalUrl});
    rejectAdditionalVerification(page.finalUrl);
  }
  if (!helpers.isPasswordLoginPage(page.finalUrl)) throw new Error('interaction_required');

  diagnostic.mark('web_login');
  let payload = await helpers.verifyPassword(client, {authBase, rl, deviceId, password, referer: page.finalUrl});
  rejectAdditionalVerification(helpers.getContinueUrl(payload), payload);
  const mfaRequired = helpers.isMfaChallengePayload(payload);
  if (mfaRequired && !totpSecret) throw new Error('interaction_required');
  payload = await helpers.completeTotpMfaIfNeeded(client, {
    authBase, rl, deviceId, payload, totpSecret, referer: page.finalUrl,
  });
  rejectAdditionalVerification(helpers.getContinueUrl(payload), payload);
  if (helpers.isMfaChallengePayload(payload)) throw new Error('mfa_required');

  if (Array.isArray(payload?.['oai-client-auth-session']?.workspaces)) diagnostic.mark('workspace_select');
  const workspaceId = helpers.pickWorkspaceId(payload);
  if (workspaceId) {
    const referer = helpers.getContinueUrl(payload) || `${authBase}/sign-in-with-chatgpt/codex/consent`;
    const selected = await helpers.authJsonStep(client, authBase, 'POST', '/api/accounts/workspace/select',
      {workspace_id: workspaceId}, {referer});
    payload = selected.data;
    rejectAdditionalVerification(helpers.getContinueUrl(payload), payload);
  }
  const continued = await helpers.continueFlow(client, payload);
  const callbackUrl = continued?.finalUrl || helpers.getContinueUrl(payload);
  rejectAdditionalVerification(callbackUrl);
  if (helpers.isAuthLoginPage(callbackUrl)) throw new Error('session_unavailable');
  diagnostic.mark('callback_validation');
  const oauth = {callbackUrl, codeVerifier, state, workspaceId};
  validateCodexCallback(oauth, redirectUri);
  client.codexPasswordOAuth = oauth;
  return {deviceId, callbackUrl, emailVerified: true, loginMethod: 'password', mfaVerified: mfaRequired};
}
