import { Button, Tooltip, TooltipContent, TooltipTrigger } from '@oakestra/ui';
import { MaximizeIcon, MinusIcon, PlusIcon } from 'lucide-react';
import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type ComponentProps,
  type PointerEvent,
  type ReactElement,
  type ReactNode,
} from 'react';

/** A kit tooltip around one focusable element, for icon buttons and terse badges. */
export function Hint({
  label,
  side = 'top',
  children,
}: {
  label: ReactNode;
  side?: ComponentProps<typeof TooltipContent>['side'];
  children: ReactElement;
}) {
  return (
    <Tooltip>
      <TooltipTrigger asChild>{children}</TooltipTrigger>
      <TooltipContent side={side} sideOffset={6} className="oai:max-w-72">
        {label}
      </TooltipContent>
    </Tooltip>
  );
}

interface View {
  x: number;
  y: number;
  scale: number;
}

const MIN_SCALE = 0.25;
const MAX_SCALE = 1.5;
const DOT_GAP = 28;

const clamp = (scale: number) => Math.min(Math.max(scale, MIN_SCALE), MAX_SCALE);

/** Zooms by `factor` keeping the canvas point under (px, py) where it is on screen. */
function zoomAt(view: View, factor: number, px: number, py: number): View {
  const scale = clamp(view.scale * factor);
  const ratio = scale / view.scale;
  return { scale, x: px - (px - view.x) * ratio, y: py - (py - view.y) * ratio };
}

/**
 * Pan and zoom surface. `children` live in canvas coordinates, `overlay` stays fixed on top.
 */
export function Canvas({
  label,
  contentWidth,
  contentHeight,
  insetLeft = 0,
  children,
  overlay,
}: {
  label: string;
  contentWidth: number;
  contentHeight: number;
  /** Width the overlay covers on the left, kept clear when fitting the content. */
  insetLeft?: number;
  children: ReactNode;
  overlay?: ReactNode;
}) {
  const frame = useRef<HTMLDivElement>(null);
  const [view, setView] = useState<View>({ x: insetLeft + 24, y: 24, scale: 1 });
  const [panning, setPanning] = useState(false);
  const fitted = useRef(false);

  const fit = useCallback(() => {
    const el = frame.current;
    if (!el || contentWidth === 0) return;
    const margin = 32;
    const width = el.clientWidth - insetLeft - 2 * margin;
    // The bottom margin is doubled to stay clear of the bar floating there.
    const height = el.clientHeight - 3 * margin;
    if (width <= 0 || height <= 0) return;
    const scale = clamp(Math.min(width / contentWidth, height / contentHeight, 1));
    setView({
      scale,
      x: insetLeft + margin + (width - contentWidth * scale) / 2,
      y: margin + Math.max((height - contentHeight * scale) / 2, 0),
    });
  }, [contentWidth, contentHeight, insetLeft]);

  // Fit once, when the frame has a size and there is something to show. Later data refreshes
  // must not move the canvas under the user's hands.
  useEffect(() => {
    if (fitted.current || contentWidth === 0 || !frame.current?.clientWidth) return;
    fitted.current = true;
    fit();
  }, [fit, contentWidth]);

  // React registers wheel listeners as passive, and a passive listener cannot stop the page
  // from scrolling (or the browser from zooming) while the canvas handles the gesture.
  useEffect(() => {
    const el = frame.current;
    if (!el) return;
    const onWheel = (event: WheelEvent) => {
      // A scrollable overlay (the model list) keeps its own wheel.
      if ((event.target as Element).closest('[data-canvas-scroll]')) return;
      event.preventDefault();
      const rect = el.getBoundingClientRect();
      setView((current) =>
        event.ctrlKey || event.metaKey
          ? zoomAt(current, Math.exp(-event.deltaY * 0.01), event.clientX - rect.left, event.clientY - rect.top)
          : { ...current, x: current.x - event.deltaX, y: current.y - event.deltaY },
      );
    };
    el.addEventListener('wheel', onWheel, { passive: false });
    return () => el.removeEventListener('wheel', onWheel);
  }, []);

  const zoomCentered = (factor: number) => {
    const el = frame.current;
    if (el) setView((current) => zoomAt(current, factor, el.clientWidth / 2, el.clientHeight / 2));
  };

  const pan = {
    onPointerDown: (event: PointerEvent<HTMLDivElement>) => {
      // Only the bare surface pans; cards and overlays keep their own gestures.
      if (event.button !== 0 || !(event.target as Element).hasAttribute('data-canvas-surface')) return;
      event.currentTarget.setPointerCapture(event.pointerId);
      setPanning(true);
    },
    onPointerMove: (event: PointerEvent<HTMLDivElement>) => {
      if (!panning) return;
      setView((current) => ({ ...current, x: current.x + event.movementX, y: current.y + event.movementY }));
    },
    onPointerUp: () => setPanning(false),
    onPointerCancel: () => setPanning(false),
  };

  // Zooming out would pack the dots into a grey haze, so the grid thins out instead: every
  // other dot is dropped until they are comfortably apart again.
  let gap = DOT_GAP * view.scale;
  while (gap < DOT_GAP * 0.75) gap *= 2;
  return (
    <div
      ref={frame}
      role="region"
      aria-label={label}
      className={`oai:relative oai:h-full oai:touch-none oai:overflow-hidden oai:text-muted-foreground/20 ${
        panning ? 'oai:cursor-grabbing' : 'oai:cursor-grab'
      }`}
      {...pan}
    >
      <div
        data-canvas-surface
        className="oai:absolute oai:inset-0"
        style={{
          backgroundImage: 'radial-gradient(currentColor 1px, transparent 1px)',
          backgroundSize: `${gap}px ${gap}px`,
          backgroundPosition: `${view.x}px ${view.y}px`,
        }}
      />
      <div
        // Zero-sized so it never covers the surface: only the cards inside take pointer events.
        className="oai:absolute oai:top-0 oai:left-0 oai:size-0 oai:cursor-default oai:text-foreground"
        style={{
          transform: `translate(${view.x}px, ${view.y}px) scale(${view.scale})`,
          transformOrigin: '0 0',
        }}
      >
        {children}
      </div>
      <div className="oai:pointer-events-none oai:absolute oai:inset-0 oai:cursor-default oai:text-foreground">
        {overlay}
        <div
          role="toolbar"
          aria-label="Zoom"
          className="oai:pointer-events-auto oai:absolute oai:bottom-4 oai:left-4 oai:flex oai:items-center oai:gap-0.5 oai:rounded-xl oai:border oai:bg-card oai:p-1 oai:shadow-lg"
        >
          <Hint label="Zoom out">
            <Button
              size="icon-sm"
              variant="ghost"
              aria-label="Zoom out"
              disabled={view.scale <= MIN_SCALE}
              onClick={() => zoomCentered(1 / 1.25)}
            >
              <MinusIcon aria-hidden />
            </Button>
          </Hint>
          <Hint label="Reset to 100%">
            <Button
              size="sm"
              variant="ghost"
              aria-label={`Zoom ${Math.round(view.scale * 100)}%, reset to 100%`}
              className="oai:w-12 oai:px-0 oai:text-xs oai:tabular-nums"
              onClick={() => zoomCentered(1 / view.scale)}
            >
              {Math.round(view.scale * 100)}%
            </Button>
          </Hint>
          <Hint label="Zoom in">
            <Button
              size="icon-sm"
              variant="ghost"
              aria-label="Zoom in"
              disabled={view.scale >= MAX_SCALE}
              onClick={() => zoomCentered(1.25)}
            >
              <PlusIcon aria-hidden />
            </Button>
          </Hint>
          <div aria-hidden className="oai:mx-0.5 oai:h-5 oai:w-px oai:bg-border" />
          <Hint label="Fit to screen">
            <Button size="icon-sm" variant="ghost" aria-label="Fit to screen" onClick={fit}>
              <MaximizeIcon aria-hidden />
            </Button>
          </Hint>
        </div>
      </div>
    </div>
  );
}
