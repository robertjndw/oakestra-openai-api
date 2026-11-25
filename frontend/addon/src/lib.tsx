import { StatusBadge, type StatusTone } from '@oakestra/ui';

import type { DeploymentState } from './types';

export function formatMb(mb: number): string {
  return mb >= 1024 ? `${(mb / 1024).toFixed(mb >= 10 * 1024 ? 0 : 1)} GB` : `${mb} MB`;
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
