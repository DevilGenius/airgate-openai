import type { UsageRecordSurfaceProps } from '@devilgenius/airgate-theme/plugin';
import type { CSSProperties } from 'react';

type UsageContext = {
  reasoning_effort?: string;
  service_tier?: string;
  usage_metadata?: Record<string, string>;
};

const EFFORT_LOW_COLOR = 'rgb(34,197,94)';
const EFFORT_MEDIUM_COLOR = 'rgb(59,130,246)';
const EFFORT_HIGH_COLOR = 'rgb(249,115,22)';
const EFFORT_XHIGH_COLOR = 'rgb(239,68,68)';

const EFFORT_COLORS: Record<string, string> = {
  low: EFFORT_LOW_COLOR,
  medium: EFFORT_MEDIUM_COLOR,
  high: EFFORT_HIGH_COLOR,
  xhigh: EFFORT_XHIGH_COLOR,
};
const IMAGE_SIZE_COLOR = 'rgb(148,163,184)';
const FAST_SERVICE_TIER_COLOR = 'rgb(168, 85, 247)';
const IMAGE_TIER_1K_MAX_PIXELS = 1536 * 1024;
const IMAGE_TIER_2K_MAX_PIXELS = 2048 * 2048;
const FAST_INDICATOR_STYLE: CSSProperties = {
  position: 'absolute',
  left: '0.375rem',
  top: 0,
  bottom: 0,
  display: 'inline-flex',
  alignItems: 'center',
  justifyContent: 'center',
  width: 'var(--ag-usage-image-dot-size, 0.375rem)',
  color: 'rgb(234, 179, 8)',
  fontSize: '0.75rem',
  height: '100%',
  lineHeight: 1,
  pointerEvents: 'none',
};

function imageSizeTier(imageSize: string): 'high' | 'low' | 'medium' {
  const normalized = imageSize.trim().toLowerCase();
  if (/\b4k\b/.test(normalized)) return 'high';
  if (/\b2k\b/.test(normalized)) return 'medium';
  if (/\b1k\b/.test(normalized)) return 'low';

  const dimensions = normalized.match(/\d+(?:\.\d+)?/g)?.map(Number).filter(Number.isFinite) ?? [];
  const [width, height] = dimensions;
  if (width && height) {
    const pixels = width * height;
    if (pixels > IMAGE_TIER_2K_MAX_PIXELS) return 'high';
    if (pixels > IMAGE_TIER_1K_MAX_PIXELS) return 'medium';
  }
  return 'low';
}

function imageSizeDotColor(imageSize: string): string {
  const tier = imageSizeTier(imageSize);
  if (tier === 'high') return EFFORT_HIGH_COLOR;
  if (tier === 'medium') return EFFORT_MEDIUM_COLOR;
  return EFFORT_LOW_COLOR;
}

function chipStyle(color: string): CSSProperties {
  return {
    background: `color-mix(in srgb, ${color} 18%, transparent)`,
    boxShadow: `inset 0 0 0 1px color-mix(in srgb, ${color} 34%, transparent)`,
    color,
  };
}

function isUsageServiceTierFast(context: UsageRecordSurfaceProps['context']): boolean {
  const serviceTier = String((context as UsageContext | undefined)?.service_tier ?? '').trim().toLowerCase();
  return serviceTier === 'fast' || serviceTier === 'priority' || serviceTier === 'scale';
}

function usageMetadata(context: UsageRecordSurfaceProps['context']): Record<string, string> {
  const ctx = (context ?? {}) as UsageContext;
  const metadata = ctx.usage_metadata;
  return metadata && typeof metadata === 'object' && !Array.isArray(metadata) ? metadata : {};
}

export function UsageModelMeta(props: UsageRecordSurfaceProps) {
  const ctx = (props.context ?? {}) as UsageContext;
  const imageSize = usageMetadata(props.context)['openai.image.size']?.trim() ?? '';
  const chips: Array<{ imageTier?: 'high' | 'low' | 'medium'; label: string; color: string; dotColor?: string; fastMark?: boolean }> = [];

  if (imageSize) {
    chips.push({
      label: imageSize,
      color: IMAGE_SIZE_COLOR,
      dotColor: imageSizeDotColor(imageSize),
      imageTier: imageSizeTier(imageSize),
    });
  }
  const hasReasoningEffort = Boolean(ctx.reasoning_effort?.trim());
  const showFastMark = !imageSize && isUsageServiceTierFast(ctx);
  if (showFastMark && !hasReasoningEffort) {
    chips.push({ label: 'fast', color: FAST_SERVICE_TIER_COLOR, fastMark: true });
  }
  if (ctx.reasoning_effort) {
    chips.push({
      label: ctx.reasoning_effort,
      color: EFFORT_COLORS[ctx.reasoning_effort] ?? 'rgb(148,163,184)',
      fastMark: showFastMark,
    });
  }

  if (!chips.length) return null;

  return (
    <div className="flex shrink-0 gap-1">
      {chips.map((chip) => (
        <span
          key={chip.label}
          className={[
            'ag-usage-meta-chip',
            chip.dotColor && 'ag-usage-meta-chip--image',
            chip.imageTier && `ag-usage-meta-chip--image-${chip.imageTier}`,
            'inline-flex shrink-0 items-center rounded px-1.5 font-semibold leading-4 whitespace-nowrap',
            chip.dotColor ? 'justify-start gap-1 text-[11px]' : 'text-[12px]',
          ].filter(Boolean).join(' ')}
          style={{
            ...chipStyle(chip.color),
            '--ag-usage-meta-chip-color': chip.color,
            '--ag-usage-meta-chip-dot-color': chip.dotColor ?? chip.color,
            position: chip.fastMark ? 'relative' : undefined,
          } as CSSProperties}
        >
          {chip.dotColor ? (
            <span
              className="ag-usage-meta-chip-dot"
              aria-hidden="true"
              style={{ backgroundColor: chip.dotColor }}
            />
          ) : null}
          {chip.fastMark ? <span aria-hidden="true" style={FAST_INDICATOR_STYLE}>⚡️</span> : null}
          {chip.label}
        </span>
      ))}
    </div>
  );
}
