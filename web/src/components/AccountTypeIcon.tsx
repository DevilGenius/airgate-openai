import type { CSSProperties } from 'react';

const style: CSSProperties = {
  display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
  width: '1.25rem', height: '1.25rem', flexShrink: 0, borderRadius: '0.375rem',
  background: 'var(--ag-bg-surface)', color: 'var(--ag-text-secondary)',
};

// 静态 SVG + 原生 title，列表中不创建逐行订阅、浮层或事件监听。
const glyphs = {
  oauth: <><path d="M10 13a5 5 0 0 0 7.1 0l3-3a5 5 0 0 0-7.1-7.1l-1.7 1.7" /><path d="M14 11a5 5 0 0 0-7.1 0l-3 3a5 5 0 0 0 7.1 7.1l1.7-1.7" /></>,
  apikey: <><circle cx="16" cy="8" r="5" /><path d="m12.5 11.5-9 9H3v-4l3-3h3v-3" /></>,
  session_key: <><rect x="3" y="4" width="18" height="16" rx="3" /><path d="M3 9h18m-13 5h8m-8 3h4" /></>,
  identity: <><path d="M8 12a4 4 0 0 1 8 0c0 4-1 7-3 9M4 12a8 8 0 0 1 16 0m-8 0c0 4-1 6-3 8M4 16l-1 3m17-3-1 4" /></>,
};

export function AccountTypeIcon({ type, label }: { type: string; label?: string }) {
  const name = label ?? ({ oauth: 'OAuth', apikey: 'API Key', session_key: 'Session Key' }[type] ?? type);
  const glyph = glyphs[type as keyof typeof glyphs] ?? glyphs.identity;
  return (
    <span style={style} role="img" aria-label={name} title={name}>
      <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" focusable="false">
        {glyph}
      </svg>
    </span>
  );
}
