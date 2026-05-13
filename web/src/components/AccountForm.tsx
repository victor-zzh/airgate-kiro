import { useState, useCallback, useEffect, useRef } from 'react';
import { cssVar } from '@doudou-start/airgate-theme';

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
    start: () => Promise<{ authorizeURL: string; state: string; autoCallback?: boolean }>;
    exchange: (callbackURL: string) => Promise<{
      accountType: string;
      accountName: string;
      credentials: Record<string, string>;
      status?: string;
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

type AccountType = 'oauth' | 'api_key';

export function AccountForm({
  credentials,
  onChange,
  mode,
  accountType,
  onAccountTypeChange,
  onSuggestedName,
  onBatchModeChange,
  onBatchImport,
  oauth,
}: AccountFormProps) {
  const currentType = (accountType || 'oauth') as AccountType;
  const [batchMode, setBatchMode] = useState(false);
  const [batchJson, setBatchJson] = useState('');
  const [batchStatus, setBatchStatus] = useState<{ type: 'info' | 'success' | 'error'; text: string } | null>(null);
  const [batchLoading, setBatchLoading] = useState(false);

  const [authorizeURL, setAuthorizeURL] = useState('');
  const [callbackURL, setCallbackURL] = useState('');
  const [oauthLoading, setOauthLoading] = useState(false);
  const [oauthStatus, setOauthStatus] = useState<{ type: 'info' | 'success' | 'error'; text: string } | null>(null);
  const [deviceAuth, setDeviceAuth] = useState<{ verificationURI: string; userCode: string; sessionID: string } | null>(null);
  const [polling, setPolling] = useState(false);
  const [showManualPaste, setShowManualPaste] = useState(false);
  const pollRef = useRef<ReturnType<typeof setInterval> | null>(null);
  const sessionRef = useRef<string>('');

  const stopPolling = useCallback(() => {
    if (pollRef.current) {
      clearInterval(pollRef.current);
      pollRef.current = null;
    }
    setPolling(false);
  }, []);

  useEffect(() => {
    return () => { if (pollRef.current) clearInterval(pollRef.current); };
  }, []);

  const handleOAuthResult = useCallback((result: { accountType: string; accountName: string; credentials: Record<string, string> }) => {
    if (result.accountType === '__device_auth__') {
      const uri = result.credentials?.verification_uri;
      const code = result.credentials?.user_code;
      const sid = result.credentials?.session_id;
      if (uri && code && sid) {
        setDeviceAuth({ verificationURI: uri, userCode: code, sessionID: sid });
        setAuthorizeURL('');
        setCallbackURL('');
        setShowManualPaste(false);
        setOauthStatus({ type: 'info', text: `请在浏览器中打开验证链接，输入验证码 ${code} 完成授权。` });
        return;
      }
    }
    onAccountTypeChange?.(result.accountType || 'oauth');
    onChange({ ...credentials, ...result.credentials });
    if (result.accountName) onSuggestedName?.(result.accountName);
    setOauthStatus({ type: 'success', text: '授权成功！凭证已自动填充，请点击创建完成添加。' });
    setAuthorizeURL('');
    setShowManualPaste(false);
    setDeviceAuth(null);
  }, [onAccountTypeChange, onChange, credentials, onSuggestedName]);

  const selectType = useCallback((type: AccountType) => {
    onAccountTypeChange?.(type);
    setAuthorizeURL('');
    setCallbackURL('');
    setOauthStatus(null);
    setShowManualPaste(false);
    stopPolling();
  }, [onAccountTypeChange, stopPolling]);

  // ── OAuth 浏览器流程 ──
  const startOAuth = useCallback(async () => {
    if (!oauth) return;
    stopPolling();
    setOauthLoading(true);
    setShowManualPaste(false);
    setOauthStatus({ type: 'info', text: '正在生成授权链接...' });
    try {
      const result = await oauth.start();
      setAuthorizeURL(result.authorizeURL);
      setCallbackURL('');
      sessionRef.current = result.state;

      if (result.autoCallback) {
        setOauthStatus({ type: 'info', text: '请复制授权链接在浏览器中打开，登录后将自动获取凭证...' });
        setPolling(true);
        pollRef.current = setInterval(async () => {
          try {
            const pollResult = await oauth.exchange('poll:' + sessionRef.current);
            if (pollResult.status === 'complete') {
              stopPolling();
              handleOAuthResult(pollResult);
            } else if (pollResult.status === 'device_auth') {
              stopPolling();
              handleOAuthResult(pollResult);
            } else if (pollResult.status === 'unavailable') {
              stopPolling();
              setShowManualPaste(true);
              setOauthStatus({ type: 'info', text: '自动捕获不可用，请手动粘贴回调 URL。' });
            }
          } catch {
            // 轮询错误不中断，继续等待
          }
        }, 2000);

        // 5 分钟超时
        setTimeout(() => {
          if (pollRef.current) {
            stopPolling();
            setShowManualPaste(true);
            setOauthStatus({ type: 'info', text: '自动捕获超时，请手动粘贴回调 URL。' });
          }
        }, 5 * 60 * 1000);
      } else {
        setShowManualPaste(true);
        setOauthStatus({ type: 'info', text: '请复制授权链接在浏览器中打开，登录后请复制地址栏 URL 粘贴到下方。' });
      }
    } catch (error) {
      setOauthStatus({ type: 'error', text: error instanceof Error ? error.message : '生成授权链接失败' });
    } finally {
      setOauthLoading(false);
    }
  }, [oauth, stopPolling, handleOAuthResult]);

  const submitOAuthCallback = useCallback(async () => {
    if (!oauth || !callbackURL.trim()) return;
    stopPolling();
    setOauthLoading(true);
    setOauthStatus({ type: 'info', text: '正在完成授权交换...' });
    try {
      const result = await oauth.exchange(callbackURL.trim());
      handleOAuthResult(result);
    } catch (error) {
      setOauthStatus({ type: 'error', text: error instanceof Error ? error.message : '授权交换失败' });
    } finally {
      setOauthLoading(false);
    }
  }, [oauth, callbackURL, stopPolling, handleOAuthResult]);

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
            ['oauth', 'OAuth 授权', '浏览器登录或批量导入凭证'],
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
          <div style={{ display: 'flex', gap: '0.5rem', flexWrap: 'wrap', alignItems: 'center' }}>
            <button type="button" onClick={() => { setBatchMode(false); onBatchModeChange?.(false); startOAuth(); }} disabled={oauthLoading || polling || batchMode} style={primaryBtn(oauthLoading || polling || batchMode)}>
              {polling ? '等待授权中...' : oauthLoading ? '处理中...' : '生成授权链接'}
            </button>
            {onBatchImport && (
              <button type="button" onClick={() => { setBatchMode(!batchMode); onBatchModeChange?.(!batchMode); stopPolling(); setOauthStatus(null); }} style={outlineBtn(false)}>
                {batchMode ? '返回授权' : '批量导入'}
              </button>
            )}
            {authorizeURL && (
              <button type="button" onClick={copyURL} disabled={oauthLoading} style={outlineBtn(oauthLoading)}>
                复制链接
              </button>
            )}
            {polling && (
              <button type="button" onClick={() => { stopPolling(); setShowManualPaste(true); setOauthStatus({ type: 'info', text: '已切换为手动模式，请粘贴回调 URL。' }); }} style={outlineBtn(false)}>
                手动粘贴
              </button>
            )}
          </div>

          {/* 自动捕获等待中的动画指示 */}
          {polling && (
            <div style={{
              display: 'flex', alignItems: 'center', gap: '0.5rem',
              padding: '0.5rem 0.75rem', borderRadius: cssVar('radiusMd'),
              backgroundColor: `color-mix(in oklab, ${cssVar('info')} 8%, transparent)`,
              fontSize: '0.8rem', color: cssVar('info'),
            }}>
              <span style={{ display: 'inline-block', width: 8, height: 8, borderRadius: '50%', backgroundColor: cssVar('info'), animation: 'pulse 1.5s ease-in-out infinite' }} />
              正在等待浏览器授权完成，凭证将自动填充...
              <style>{`@keyframes pulse { 0%, 100% { opacity: 1; } 50% { opacity: 0.3; } }`}</style>
            </div>
          )}

          {/* 手动粘贴回调 URL（自动捕获不可用时或用户主动切换） */}
          {showManualPaste && !deviceAuth && (
            <div style={{ display: 'flex', flexDirection: 'column', gap: '0.5rem' }}>
              {authorizeURL && (
                <>
                  <label style={labelStyle}>授权链接（已在新窗口中打开）</label>
                  <input type="text" readOnly value={authorizeURL} style={{ ...inputStyle, fontSize: '0.75rem' }} onClick={(e) => (e.target as HTMLInputElement).select()} />
                </>
              )}

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

      {/* 批量导入（OAuth 创建模式下） */}
      {currentType === 'oauth' && mode === 'create' && onBatchImport && batchMode && (
        <div style={{ display: 'flex', flexDirection: 'column', gap: '0.5rem' }}>
          <label style={labelStyle}>JSON 批量导入</label>
              <textarea
                rows={10}
                placeholder={`粘贴 JSON 数组，每项包含 refresh_token、client_id、client_secret，例如：
[
  {
    "name": "账号1",
    "refresh_token": "...",
    "client_id": "...",
    "client_secret": "...",
    "region": "us-east-1"
  },
  {
    "name": "账号2",
    "refresh_token": "...",
    "client_id": "...",
    "client_secret": "..."
  }
]

也支持 OAuth 账号格式（无需 client_id/client_secret）：
{ "name": "...", "type": "oauth", "refresh_token": "..." }`}
                value={batchJson}
                onChange={(e) => { setBatchJson(e.target.value); setBatchStatus(null); }}
                style={{ ...inputStyle, fontFamily: 'monospace', fontSize: '0.8rem', resize: 'vertical' }}
              />
              {(() => {
                if (!batchJson.trim()) return null;
                try {
                  const parsed = JSON.parse(batchJson.trim());
                  const items = Array.isArray(parsed) ? parsed : [parsed];
                  const valid = items.filter((it: Record<string, string>) => it.refresh_token);
                  return (
                    <div style={{ fontSize: '0.8rem', color: cssVar('textSecondary') }}>
                      解析到 {valid.length} 个有效账号{valid.length < items.length ? `（${items.length - valid.length} 个缺少 refresh_token 已跳过）` : ''}
                    </div>
                  );
                } catch {
                  return <div style={{ fontSize: '0.8rem', color: cssVar('danger') }}>JSON 格式错误</div>;
                }
              })()}
              <button
                type="button"
                disabled={batchLoading || !batchJson.trim()}
                style={{
                  ...inputStyle,
                  cursor: batchLoading || !batchJson.trim() ? 'not-allowed' : 'pointer',
                  backgroundColor: cssVar('primary'),
                  color: cssVar('primaryForeground'),
                  borderWidth: 0,
                  borderStyle: 'none',
                  fontWeight: 500,
                  width: 'auto',
                  opacity: batchLoading || !batchJson.trim() ? 0.6 : 1,
                }}
                onClick={async () => {
                  if (!onBatchImport) return;
                  setBatchLoading(true);
                  setBatchStatus({ type: 'info', text: '正在导入...' });
                  try {
                    const parsed = JSON.parse(batchJson.trim());
                    const items: Record<string, string>[] = Array.isArray(parsed) ? parsed : [parsed];
                    const accounts = items
                      .filter((it) => it.refresh_token)
                      .map((it, i) => {
                        const creds: Record<string, string> = { refresh_token: it.refresh_token };
                        if (it.client_id) creds.client_id = it.client_id;
                        if (it.client_secret) creds.client_secret = it.client_secret;
                        if (it.region) creds.region = it.region;
                        return {
                          name: it.name || `Kiro-${i + 1}`,
                          type: 'oauth',
                          credentials: creds,
                        };
                      });
                    if (accounts.length === 0) {
                      setBatchStatus({ type: 'error', text: '没有找到有效账号（需包含 refresh_token）' });
                      return;
                    }
                    const result = await onBatchImport(accounts);
                    setBatchStatus({ type: 'success', text: `导入完成：成功 ${result.imported} 个，失败 ${result.failed} 个` });
                    if (result.imported > 0) setBatchJson('');
                  } catch (err) {
                    setBatchStatus({ type: 'error', text: err instanceof Error ? err.message : '导入失败' });
                  } finally {
                    setBatchLoading(false);
                  }
                }}
              >
                {batchLoading ? '导入中...' : '开始导入'}
              </button>
              {batchStatus && (
                <div style={{
                  padding: '0.5rem 0.75rem',
                  borderRadius: cssVar('radiusMd'),
                  fontSize: '0.8rem',
                  color: statusColors[batchStatus.type],
                  backgroundColor: `color-mix(in oklab, ${statusColors[batchStatus.type]} 10%, transparent)`,
                  borderWidth: '1px',
                  borderStyle: 'solid',
                  borderColor: `color-mix(in oklab, ${statusColors[batchStatus.type]} 25%, transparent)`,
                }}>
                  {batchStatus.text}
                </div>
              )}
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
