import type { CSSProperties, ReactNode } from 'react';
import type { UsageRecordSurfaceProps } from '@devilgenius/airgate-theme/plugin';

interface UsageCostDetailItem {
  key?: string;
  label?: string;
  account_cost?: number;
  user_cost?: number;
  billing_multiplier?: number;
  currency?: string;
}

interface UsageRecordLike {
  model?: string;
  input_cost?: number;
  output_cost?: number;
  cached_input_cost?: number;
  cache_creation_cost?: number;
  total_cost?: number;
  actual_cost?: number;
  billed_cost?: number;
  cost?: number;
  account_cost?: number;
  rate_multiplier?: number;
  sell_rate?: number;
  effective_rate?: number;
  account_rate_multiplier?: number;
  service_tier?: string;
  input_price?: number;
  output_price?: number;
  cached_input_price?: number;
  cache_creation_price?: number;
  usage_metadata?: Record<string, string>;
}

const panelStyle: CSSProperties = {
  overflow: 'hidden',
  borderRadius: 'var(--radius)',
};

const headerStyle: CSSProperties = {
  borderBottom: '1px solid var(--ag-border)',
  background: 'var(--ag-default-bg)',
  padding: 'var(--ag-usage-tooltip-header-padding, 0.25rem 0.5rem)',
};

const titleStyle: CSSProperties = {
  color: 'var(--ag-text)',
  fontSize: '0.875rem',
  fontWeight: 600,
  lineHeight: 1,
};

const subtitleStyle: CSSProperties = {
  marginTop: 'var(--ag-usage-tooltip-subtitle-margin-top, 0.125rem)',
  overflow: 'hidden',
  color: 'var(--ag-text-tertiary)',
  fontSize: '0.75rem',
  lineHeight: 'var(--ag-usage-tooltip-subtitle-line-height, 0.875rem)',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
};

const bodyStyle: CSSProperties = {
  display: 'flex',
  flexDirection: 'column',
  gap: 'var(--ag-usage-tooltip-body-gap, 0.0625rem)',
  padding: 'var(--ag-usage-tooltip-body-padding, 0.25rem)',
};

const rowStyle: CSSProperties = {
  display: 'grid',
  gridTemplateColumns: 'minmax(0,1fr) minmax(7rem,max-content)',
  alignItems: 'center',
  gap: 'var(--ag-usage-tooltip-row-column-gap, 0.5rem)',
  borderRadius: 'var(--radius)',
  background: 'var(--ag-surface)',
  padding: 'var(--ag-usage-tooltip-row-padding, 0.125rem 0.375rem)',
  fontSize: '0.75rem',
  lineHeight: 'var(--ag-usage-tooltip-row-line-height, 0.875rem)',
};

const labelStyle: CSSProperties = {
  minWidth: 0,
  overflow: 'hidden',
  color: 'var(--ag-text-tertiary)',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
};

const valueStyle: CSSProperties = {
  minWidth: 0,
  maxWidth: '12rem',
  justifySelf: 'end',
  overflow: 'hidden',
  color: 'var(--ag-text-secondary)',
  fontFamily: 'var(--ag-font-mono)',
  fontWeight: 500,
  textAlign: 'right',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
};

const dividerStyle: CSSProperties = {
  margin: 'var(--ag-usage-tooltip-divider-margin, 0.0625rem 0)',
  borderTop: '1px solid var(--ag-border)',
};

function recordFromContext(context: UsageRecordSurfaceProps['context']): UsageRecordLike {
  const record = context?.record;
  return record && typeof record === 'object' ? record as UsageRecordLike : {};
}

function metadataFromContext(context: UsageRecordSurfaceProps['context'], record: UsageRecordLike): Record<string, string> {
  const fromContext = context?.usage_metadata;
  if (fromContext && typeof fromContext === 'object' && !Array.isArray(fromContext)) {
    return fromContext as Record<string, string>;
  }
  return record.usage_metadata ?? {};
}

function metadataText(metadata: Record<string, string>, key: string) {
  const value = metadata[key]?.trim();
  return value || '';
}

function money(value: unknown) {
  const amount = typeof value === 'number' && Number.isFinite(value) ? value : 0;
  return `$${amount.toFixed(6)}`;
}

function finiteNumber(value: unknown): number | undefined {
  return typeof value === 'number' && Number.isFinite(value) ? value : undefined;
}

function rate(value: unknown) {
  const amount = finiteNumber(value);
  if (amount === undefined) return '-';
  return `${amount.toLocaleString(undefined, {
    maximumFractionDigits: 3,
    minimumFractionDigits: 0,
    useGrouping: false,
  })}x`;
}

function Row({ label, tone, value }: { label: ReactNode; tone?: string; value: ReactNode }) {
  return (
    <div style={rowStyle}>
      <span style={labelStyle}>{label}</span>
      <span style={{ ...valueStyle, color: tone }}>{value}</span>
    </div>
  );
}

function stripTokenSuffix(s: string): string {
  return s.replace(/\s*Token\s*$/i, '').replace(/\s*成本\s*$/, '').trim();
}

function toCostLabel(raw: string): string {
  const s = raw.trim();
  if (s.includes('成本') || s.includes('费用') || s.toLowerCase().includes('cost')) return s;
  return stripTokenSuffix(s) + '成本';
}

function fallbackDetails(record: UsageRecordLike): UsageCostDetailItem[] {
  return [
    { key: 'input_tokens', label: '输入', account_cost: record.input_cost },
    { key: 'cached_input_tokens', label: '缓存输入', account_cost: record.cached_input_cost },
    { key: 'cache_creation_tokens', label: '缓存写入', account_cost: record.cache_creation_cost },
    { key: 'output_tokens', label: '输出', account_cost: record.output_cost },
  ].filter((item) => (item.account_cost ?? 0) > 0);
}

export function UsageCostDetail({ context }: UsageRecordSurfaceProps) {
  const record = recordFromContext(context);
  const metadata = metadataFromContext(context, record);
  const isAdmin = context?.adminView !== false;
  const groupRate = finiteNumber(record.rate_multiplier);
  const upstreamRate = finiteNumber(record.account_rate_multiplier);
  const sellRate = finiteNumber(record.sell_rate);
  const finalRate = finiteNumber(record.effective_rate);
  const keyBillingCost = record.cost ?? record.billed_cost ?? record.actual_cost;
  const showSellRate = isAdmin && sellRate !== undefined && sellRate !== 1;
  const showUserBalanceCharge = isAdmin
    && record.billed_cost !== undefined
    && record.actual_cost !== undefined
    && record.billed_cost !== record.actual_cost;

  const rows = fallbackDetails(record);

  const unitPrices: { label: string; value: string }[] = [];
  const imageUnitPrice = Number(metadataText(metadata, 'openai.image.unit_price'));
  const imageUnit = metadataText(metadata, 'openai.image.unit') || 'USD/image';
  if (Number.isFinite(imageUnitPrice) && imageUnitPrice > 0) {
    unitPrices.push({
      label: '图片单价',
      value: `$${imageUnitPrice.toFixed(4)} / ${imageUnit.replace(/^USD\//, '')}`,
    });
  }
  if (unitPrices.length === 0) {
    if (record.input_price && record.input_price > 0)
      unitPrices.push({ label: '输入单价', value: `$${record.input_price.toFixed(4)} / 1M Token` });
    if (record.cached_input_price && record.cached_input_price > 0)
      unitPrices.push({ label: '缓存读取单价', value: `$${record.cached_input_price.toFixed(4)} / 1M Token` });
    if (record.output_price && record.output_price > 0)
      unitPrices.push({ label: '输出单价', value: `$${record.output_price.toFixed(4)} / 1M Token` });
    if (record.cache_creation_price && record.cache_creation_price > 0)
      unitPrices.push({ label: '缓存写入单价', value: `$${record.cache_creation_price.toFixed(4)} / 1M Token` });
  }

  const hasRateInfo = !!record.service_tier
    || (isAdmin
      ? groupRate !== undefined || upstreamRate !== undefined || showSellRate
      : finalRate !== undefined);

  return (
    <div style={panelStyle}>
      <div style={headerStyle}>
        <div style={titleStyle}>OpenAI 费用明细</div>
        {record.model ? <div style={subtitleStyle}>{record.model}</div> : null}
      </div>
      <div style={bodyStyle}>
        {rows.map((item, index) => (
          <Row
            key={item.key || `${item.label}:${index}`}
            label={toCostLabel(item.label || item.key || '费用')}
            value={money(item.user_cost ?? item.account_cost)}
          />
        ))}
        {unitPrices.map((up, i) => (
          <Row key={`up-${i}`} label={up.label} value={up.value} />
        ))}
        <div style={dividerStyle} />
        {record.service_tier ? (
          <Row label="服务档位" value={<span style={{ textTransform: 'capitalize' }}>{record.service_tier}</span>} />
        ) : null}
        {isAdmin && groupRate !== undefined ? (
          <Row label="分组倍率" value={rate(groupRate)} />
        ) : null}
        {!isAdmin && finalRate !== undefined ? (
          <Row label="倍率" value={rate(finalRate)} />
        ) : null}
        {isAdmin && upstreamRate !== undefined ? (
          <Row label="上游倍率" value={rate(upstreamRate)} />
        ) : null}
        {showSellRate ? (
          <Row label="销售倍率" value={rate(sellRate)} />
        ) : null}
        {hasRateInfo ? <div style={dividerStyle} /> : null}
        <Row label="原始成本" value={money(record.total_cost)} tone="var(--ag-text)" />
        {isAdmin && record.account_cost !== undefined ? (
          <Row label="上游计费" value={money(record.account_cost)} tone="var(--ag-success)" />
        ) : null}
        {showUserBalanceCharge ? (
          <>
            <Row label="余额扣费" value={money(record.actual_cost)} tone="var(--ag-primary)" />
            <Row label="利润" value={money((record.billed_cost ?? 0) - (record.actual_cost ?? 0))} tone="var(--ag-success)" />
          </>
        ) : null}
        <Row label="密钥计费" value={money(keyBillingCost)} tone="var(--ag-warning)" />
      </div>
    </div>
  );
}
