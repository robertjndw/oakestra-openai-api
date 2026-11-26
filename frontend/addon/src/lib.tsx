import { formatRelativeTime, StatusBadge, type StatusTone } from '@oakestra/ui';

import type { DeploymentState } from './types';

export function formatMb(mb: number): string {
  if (mb < 1024) return `${mb} MB`;
  // One decimal below 10 GB, but 2048 reads as "2 GB", not "2.0 GB".
  return `${(mb / 1024).toFixed(mb >= 10 * 1024 ? 0 : 1).replace(/\.0$/, '')} GB`;
}

export function formatTime(ms: number | null): string {
  return ms === null ? 'never' : new Date(ms).toLocaleString();
}

/** Shared by the badge and the placement graph so both color a state alike. */
export const DEPLOYMENT_TONES: Record<DeploymentState, StatusTone> = {
  pending: 'info',
  scheduling: 'info',
  starting: 'info',
  deleting: 'info',
  ready: 'success',
  unhealthy: 'warning',
  failed: 'destructive',
};

export function DeploymentStateBadge({ state }: { state: DeploymentState }) {
  const changing = DEPLOYMENT_TONES[state] === 'info';
  return (
    <StatusBadge tone={DEPLOYMENT_TONES[state]} pulse={changing}>
      {state}
    </StatusBadge>
  );
}

/** One argument per line, the way people paste them; blank lines are dropped. */
export const splitLines = (text: string): string[] =>
  text
    .split('\n')
    .map((line) => line.trim())
    .filter(Boolean);

/** Compact "5 minutes ago" for tables; pair it with `formatTime` in a title for the exact moment. */
export function formatRelative(ms: number | null): string {
  return ms === null ? 'never' : formatRelativeTime(ms);
}
