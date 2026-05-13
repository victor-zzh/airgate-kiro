import type { AccountSurfaceProps } from '@doudou-start/airgate-theme/plugin';

interface UsageWindowItem {
  key?: string;
  label: string;
  used_percent: number;
  reset_seconds: number;
}

function getUsageWindows(context: AccountSurfaceProps['context']): UsageWindowItem[] {
  const windows = context?.windows;
  if (!Array.isArray(windows)) return [];
  return windows.filter((item): item is UsageWindowItem => (
    item !== null
    && typeof item === 'object'
    && typeof item.label === 'string'
    && typeof item.used_percent === 'number'
    && typeof item.reset_seconds === 'number'
  ));
}

function formatReset(seconds: number) {
  if (!seconds || seconds <= 0) return '-';
  const d = Math.floor(seconds / 86400);
  const h = Math.floor((seconds % 86400) / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  if (d > 0) return h > 0 ? `${d}d${h}h` : `${d}d`;
  if (h > 0) return m > 0 ? `${h}h${m}m` : `${h}h`;
  return `${m}m`;
}

function usageColor(pct: number) {
  if (pct < 50) return 'var(--ag-success)';
  if (pct < 80) return 'var(--ag-warning)';
  return 'var(--ag-danger)';
}

export function UsageWindow({ context }: AccountSurfaceProps) {
  const windows = getUsageWindows(context);
  if (windows.length === 0) return null;

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: '0.125rem', minWidth: 0, fontFamily: 'var(--ag-font-mono)' }}>
      {windows.map((w) => {
        const percent = Math.round(w.used_percent);
        const barPercent = Math.max(0, Math.min(100, percent));
        const color = usageColor(w.used_percent);
        const resetText = formatReset(w.reset_seconds);
        return (
          <div key={w.key || w.label} style={{ display: 'flex', flexDirection: 'column', gap: '0.125rem', minWidth: 0 }}>
            <span style={{ fontSize: '0.625rem', fontWeight: 600, lineHeight: 1, color: 'var(--ag-text-secondary)', whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>
              {w.label}
            </span>
            <div style={{ display: 'grid', gridTemplateColumns: 'minmax(4.625rem, 1fr) 1.375rem 2.75rem', height: '0.75rem', alignItems: 'center', gap: '0.125rem', minWidth: 0 }}>
              <div style={{ height: '0.375rem', minWidth: 0, overflow: 'hidden', borderRadius: '999px', background: 'var(--ag-glass-border)' }}>
                <div style={{ width: `${barPercent}%`, height: '100%', borderRadius: '999px', background: color }} />
              </div>
              <span style={{ width: '100%', minWidth: 0, overflow: 'hidden', textAlign: 'right', fontSize: '0.625rem', fontWeight: 600, lineHeight: 1, fontVariantNumeric: 'tabular-nums', whiteSpace: 'nowrap', color }}>
                {percent}%
              </span>
              <span style={{ display: 'inline-flex', height: '100%', minWidth: 0, alignItems: 'center', justifyContent: 'flex-end', fontSize: '0.625rem', fontWeight: 600, lineHeight: 1, fontVariantNumeric: 'tabular-nums', whiteSpace: 'nowrap', color: 'var(--ag-text-secondary)' }}>
                {resetText}
              </span>
            </div>
          </div>
        );
      })}
    </div>
  );
}
