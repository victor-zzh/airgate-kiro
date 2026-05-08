import { useState, useCallback } from 'react';
import { cssVar } from '@airgate/theme';

export interface AccountFormProps {
  credentials: Record<string, string>;
  onChange: (credentials: Record<string, string>) => void;
  mode: 'create' | 'edit';
  accountType?: string;
  onAccountTypeChange?: (type: string) => void;
  onSuggestedName?: (name: string) => void;
  onBatchModeChange?: (isBatch: boolean) => void;
  onBatchImport?: (accounts: Array<{ name: string; type: string; credentials: Record<string, string> }>) => Promise<{ imported: number; failed: number }>;
  oauth?: {
    start: () => Promise<{ authorizeURL: string; state: string }>;
    exchange: (callbackURL: string) => Promise<{
      accountType: string;
      accountName: string;
      credentials: Record<string, string>;
    }>;
  };
}

const inputStyle: React.CSSProperties = {
  display: 'block',
  width: '100%',
  borderRadius: cssVar('radiusMd'),
  borderWidth: '1px',
  borderStyle: 'solid',
  borderColor: cssVar('border'),
  backgroundColor: cssVar('fieldBackground'),
  padding: '0.5rem 0.75rem',
  fontSize: '0.875rem',
  color: cssVar('fieldForeground'),
  outline: 'none',
  transition: 'border-color 0.2s, box-shadow 0.2s',
};

const labelStyle: React.CSSProperties = {
  display: 'block',
  fontSize: '0.75rem',
  fontWeight: 500,
  color: cssVar('textSecondary'),
  textTransform: 'uppercase' as const,
  letterSpacing: '0.05em',
  marginBottom: '0.375rem',
};

const cardStyle: React.CSSProperties = {
  borderWidth: '1px',
  borderStyle: 'solid',
  borderColor: cssVar('border'),
  borderRadius: cssVar('radiusLg'),
  padding: '1rem',
  cursor: 'pointer',
  transition: 'border-color 0.2s, background-color 0.2s',
};

const cardActiveStyle: React.CSSProperties = {
  ...cardStyle,
  borderColor: cssVar('primary'),
  backgroundColor: cssVar('primarySubtle'),
};

type AccountType = 'oauth' | 'idc' | 'api_key';

export function AccountForm({
  credentials,
  onChange,
  mode,
  accountType,
  onAccountTypeChange,
  onSuggestedName,
  oauth,
}: AccountFormProps) {
  const currentType = (accountType || 'oauth') as AccountType;

  const [authorizeURL, setAuthorizeURL] = useState('');
  const [callbackURL, setCallbackURL] = useState('');
  const [oauthLoading, setOauthLoading] = useState(false);
  const [oauthStatus, setOauthStatus] = useState<{ type: 'info' | 'success' | 'error'; text: string } | null>(null);
  const [deviceAuth, setDeviceAuth] = useState<{ verificationURI: string; userCode: string; sessionID: string } | null>(null);

  const selectType = useCallback((type: AccountType) => {
    onAccountTypeChange?.(type);
    setAuthorizeURL('');
    setCallbackURL('');
    setOauthStatus(null);
  }, [onAccountTypeChange]);

  // ── OAuth 浏览器流程 ──
  const startOAuth = useCallback(async () => {
    if (!oauth) return;
    setOauthLoading(true);
    setOauthStatus({ type: 'info', text: '正在生成授权链接...' });
    try {
      const result = await oauth.start();
      setAuthorizeURL(result.authorizeURL);
      setCallbackURL('');
      setOauthStatus({ type: 'success', text: '授权链接已生成，请在浏览器中完成登录后复制地址栏 URL。' });
    } catch (error) {
      setOauthStatus({ type: 'error', text: error instanceof Error ? error.message : '生成授权链接失败' });
    } finally {
      setOauthLoading(false);
    }
  }, [oauth]);

  const submitOAuthCallback = useCallback(async () => {
    if (!oauth || !callbackURL.trim()) return;
    setOauthLoading(true);
    setOauthStatus({ type: 'info', text: '正在完成授权交换...' });
    try {
      const result = await oauth.exchange(callbackURL.trim());

      // BuilderID 设备授权：显示验证链接和验证码
      if (result.accountType === '__device_auth__') {
        const uri = result.credentials?.verification_uri;
        const code = result.credentials?.user_code;
        const sid = result.credentials?.session_id;
        if (uri && code && sid) {
          setDeviceAuth({ verificationURI: uri, userCode: code, sessionID: sid });
          setAuthorizeURL('');
          setCallbackURL('');
          setOauthStatus({ type: 'info', text: `请在浏览器中打开验证链接，输入验证码 ${code} 完成授权。` });
          return;
        }
      }

      onAccountTypeChange?.(result.accountType || 'oauth');
      onChange({ ...credentials, ...result.credentials });
      if (result.accountName) onSuggestedName?.(result.accountName);
      setOauthStatus({ type: 'success', text: '授权成功！凭证已自动填充，请点击创建完成添加。' });
      setAuthorizeURL('');
      setDeviceAuth(null);
    } catch (error) {
      setOauthStatus({ type: 'error', text: error instanceof Error ? error.message : '授权交换失败' });
    } finally {
      setOauthLoading(false);
    }
  }, [oauth, callbackURL, onAccountTypeChange, onChange, credentials, onSuggestedName]);

  const completeDeviceAuth = useCallback(async () => {
    if (!deviceAuth || !oauth) return;
    setOauthLoading(true);
    setOauthStatus({ type: 'info', text: '正在检查授权状态...' });
    try {
      const result = await oauth.exchange('device-complete:' + deviceAuth.sessionID);
      onAccountTypeChange?.(result.accountType || 'idc');
      onChange({ ...credentials, ...result.credentials });
      if (result.accountName) onSuggestedName?.(result.accountName);
      setOauthStatus({ type: 'success', text: '授权成功！凭证已自动填充，请点击创建完成添加。' });
      setDeviceAuth(null);
      setAuthorizeURL('');
    } catch (error) {
      setOauthStatus({ type: 'error', text: error instanceof Error ? error.message : '设备授权检查失败' });
    } finally {
      setOauthLoading(false);
    }
  }, [deviceAuth, oauth, onAccountTypeChange, onChange, credentials, onSuggestedName]);

  const copyURL = useCallback(async () => {
    if (!authorizeURL) return;
    try {
      await navigator.clipboard.writeText(authorizeURL);
      setOauthStatus({ type: 'success', text: '已复制到剪贴板，请在浏览器中打开。' });
    } catch {
      setOauthStatus({ type: 'error', text: '复制失败，请手动复制。' });
    }
  }, [authorizeURL]);

  const primaryBtn = (disabled: boolean): React.CSSProperties => ({
    ...inputStyle,
    cursor: disabled ? 'not-allowed' : 'pointer',
    backgroundColor: cssVar('primary'),
    color: cssVar('primaryForeground'),
    borderWidth: 0,
    borderStyle: 'none',
    fontWeight: 500,
    width: 'auto',
    opacity: disabled ? 0.6 : 1,
  });

  const outlineBtn = (disabled: boolean): React.CSSProperties => ({
    ...inputStyle,
    cursor: disabled ? 'not-allowed' : 'pointer',
    backgroundColor: 'transparent',
    borderWidth: '1px',
    borderStyle: 'solid',
    borderColor: cssVar('border'),
    fontWeight: 500,
    width: 'auto',
    opacity: disabled ? 0.6 : 1,
  });

  const statusColors: Record<string, string> = {
    info: cssVar('info'),
    success: cssVar('success'),
    error: cssVar('danger'),
  };

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: '1rem' }}>
      {/* 账号类型选择 */}
      {mode === 'create' && (
        <div style={{ display: 'flex', gap: '0.5rem', flexWrap: 'wrap' }}>
          {([
            ['oauth', 'OAuth 授权', '通过浏览器登录 Kiro'],
            ['idc', 'IdC (AWS SSO)', '手动填写 IdC 凭证'],
            ['api_key', 'API Key', 'Kiro API Key 直连'],
          ] as const).map(([type, label, desc]) => (
            <div
              key={type}
              style={currentType === type ? cardActiveStyle : cardStyle}
              onClick={() => selectType(type)}
            >
              <div style={{ fontWeight: 600, fontSize: '0.875rem' }}>{label}</div>
              <div style={{ fontSize: '0.75rem', color: cssVar('textSecondary'), marginTop: '0.25rem' }}>{desc}</div>
            </div>
          ))}
        </div>
      )}

      {/* Social OAuth - 浏览器授权 */}
      {currentType === 'oauth' && oauth && mode === 'create' && (
        <div style={{ display: 'flex', flexDirection: 'column', gap: '0.75rem' }}>
          <div style={{ display: 'flex', gap: '0.5rem', flexWrap: 'wrap' }}>
            <button type="button" onClick={startOAuth} disabled={oauthLoading} style={primaryBtn(oauthLoading)}>
              {oauthLoading ? '处理中...' : '生成授权链接'}
            </button>
            {authorizeURL && (
              <button type="button" onClick={copyURL} disabled={oauthLoading} style={outlineBtn(oauthLoading)}>
                复制链接
              </button>
            )}
          </div>

          {authorizeURL && !deviceAuth && (
            <div style={{ display: 'flex', flexDirection: 'column', gap: '0.5rem' }}>
              <label style={labelStyle}>授权链接（在浏览器中打开）</label>
              <input type="text" readOnly value={authorizeURL} style={{ ...inputStyle, fontSize: '0.75rem' }} onClick={(e) => (e.target as HTMLInputElement).select()} />

              <label style={labelStyle}>回调 URL（登录后复制浏览器地址栏的完整 URL）</label>
              <input
                type="text"
                placeholder="http://localhost:3128/oauth/callback?code=...&state=..."
                value={callbackURL}
                onChange={(e) => setCallbackURL(e.target.value)}
                style={inputStyle}
              />
              <button
                type="button"
                onClick={submitOAuthCallback}
                disabled={!callbackURL.trim() || oauthLoading}
                style={primaryBtn(!callbackURL.trim() || oauthLoading)}
              >
                完成授权
              </button>
            </div>
          )}

          {deviceAuth && (
            <div style={{ display: 'flex', flexDirection: 'column', gap: '0.5rem' }}>
              <label style={labelStyle}>验证链接（在浏览器中打开）</label>
              <input type="text" readOnly value={deviceAuth.verificationURI} style={{ ...inputStyle, fontSize: '0.75rem' }} onClick={(e) => (e.target as HTMLInputElement).select()} />
              <label style={labelStyle}>验证码</label>
              <input type="text" readOnly value={deviceAuth.userCode} style={{ ...inputStyle, fontWeight: 700, fontSize: '1.1rem', letterSpacing: '0.1em', textAlign: 'center' }} onClick={(e) => (e.target as HTMLInputElement).select()} />
              <button
                type="button"
                onClick={completeDeviceAuth}
                disabled={oauthLoading}
                style={primaryBtn(oauthLoading)}
              >
                {oauthLoading ? '检查中...' : '我已完成授权'}
              </button>
            </div>
          )}

          {oauthStatus && (
            <div style={{
              padding: '0.5rem 0.75rem',
              borderRadius: cssVar('radiusMd'),
              fontSize: '0.8rem',
              color: statusColors[oauthStatus.type],
              backgroundColor: `color-mix(in oklab, ${statusColors[oauthStatus.type]} 10%, transparent)`,
              borderWidth: '1px',
      borderStyle: 'solid',
      borderColor: `color-mix(in oklab, ${statusColors[oauthStatus.type]} 25%, transparent)`,
            }}>
              {oauthStatus.text}
            </div>
          )}

          <details style={{ fontSize: '0.8rem', color: cssVar('textSecondary') }}>
            <summary style={{ cursor: 'pointer' }}>手动填写 Refresh Token</summary>
            <div style={{ marginTop: '0.5rem', display: 'flex', flexDirection: 'column', gap: '0.5rem' }}>
              <label style={labelStyle}>Refresh Token</label>
              <input
                type="password"
                placeholder="从 Kiro IDE 本地存储中提取"
                value={credentials.refresh_token || ''}
                onChange={(e) => onChange({ ...credentials, refresh_token: e.target.value })}
                style={inputStyle}
              />
              <label style={labelStyle}>AWS Region</label>
              <input
                type="text"
                placeholder="us-east-1"
                value={credentials.region || ''}
                onChange={(e) => onChange({ ...credentials, region: e.target.value })}
                style={inputStyle}
              />
            </div>
          </details>
        </div>
      )}

      {/* Social - 编辑模式只显示字段 */}
      {currentType === 'oauth' && mode === 'edit' && (
        <div style={{ display: 'flex', flexDirection: 'column', gap: '0.5rem' }}>
          <label style={labelStyle}>Refresh Token</label>
          <input type="password" value={credentials.refresh_token || ''} onChange={(e) => onChange({ ...credentials, refresh_token: e.target.value })} style={inputStyle} />
          <label style={labelStyle}>AWS Region</label>
          <input type="text" placeholder="us-east-1" value={credentials.region || ''} onChange={(e) => onChange({ ...credentials, region: e.target.value })} style={inputStyle} />
        </div>
      )}

      {/* IdC */}
      {currentType === 'idc' && (
        <div style={{ display: 'flex', flexDirection: 'column', gap: '0.5rem' }}>
          <label style={labelStyle}>Refresh Token</label>
          <input type="password" value={credentials.refresh_token || ''} onChange={(e) => onChange({ ...credentials, refresh_token: e.target.value })} style={inputStyle} required />
          <label style={labelStyle}>Client ID</label>
          <input type="text" value={credentials.client_id || ''} onChange={(e) => onChange({ ...credentials, client_id: e.target.value })} style={inputStyle} required />
          <label style={labelStyle}>Client Secret</label>
          <input type="password" value={credentials.client_secret || ''} onChange={(e) => onChange({ ...credentials, client_secret: e.target.value })} style={inputStyle} required />
          <label style={labelStyle}>AWS Region</label>
          <input type="text" placeholder="us-east-1" value={credentials.region || ''} onChange={(e) => onChange({ ...credentials, region: e.target.value })} style={inputStyle} />
        </div>
      )}

      {/* API Key */}
      {currentType === 'api_key' && (
        <div style={{ display: 'flex', flexDirection: 'column', gap: '0.5rem' }}>
          <label style={labelStyle}>Kiro API Key</label>
          <input type="password" placeholder="ksk_..." value={credentials.kiro_api_key || ''} onChange={(e) => onChange({ ...credentials, kiro_api_key: e.target.value })} style={inputStyle} required />
          <label style={labelStyle}>AWS Region</label>
          <input type="text" placeholder="us-east-1" value={credentials.region || ''} onChange={(e) => onChange({ ...credentials, region: e.target.value })} style={inputStyle} />
        </div>
      )}
    </div>
  );
}
