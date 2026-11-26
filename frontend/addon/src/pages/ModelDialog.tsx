import {
  Alert,
  AlertDescription,
  AlertTitle,
  Button,
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  Field,
  FieldContent,
  FieldDescription,
  FieldError,
  FieldGroup,
  FieldLabel,
  FieldLegend,
  FieldSeparator,
  FieldSet,
  Input,
  Switch,
  Textarea,
  ToggleGroup,
  ToggleGroupItem,
  plural,
} from '@oakestra/ui';
import { AlertCircleIcon, ChevronDownIcon, LoaderCircleIcon } from 'lucide-react';
import { useState } from 'react';

import { useSaveModel } from '../api';
import { splitLines } from '../lib';
import type { Model } from '../types';

type MemoryUnit = 'GB' | 'MB';

interface FormState {
  name: string;
  reference: string;
  engineImage: string;
  engineArgs: string;
  command: string;
  memory: string;
  memoryUnit: MemoryUnit;
  vgpus: string;
  auto: boolean;
}

type FieldName = 'name' | 'reference' | 'memory' | 'vgpus';
type Errors = Partial<Record<FieldName, string>>;

const MB_PER_GB = 1024;

// Shown in GB unless that would need a fraction, so an odd value like 5000 MB is not turned into
// 4.8828125 GB.
function memoryFields(mb: number | undefined): Pick<FormState, 'memory' | 'memoryUnit'> {
  if (mb === undefined || mb <= 0) return { memory: '', memoryUnit: 'GB' };
  return mb % MB_PER_GB === 0
    ? { memory: String(mb / MB_PER_GB), memoryUnit: 'GB' }
    : { memory: String(mb), memoryUnit: 'MB' };
}

const toMb = (value: string, unit: MemoryUnit): number => {
  const amount = Number(value);
  return Math.round(unit === 'GB' ? amount * MB_PER_GB : amount);
};

const fromModel = (model?: Model): FormState => ({
  name: model?.name ?? '',
  reference: model?.reference ?? '',
  engineImage: model?.engine_image ?? '',
  engineArgs: model?.engine_args.join('\n') ?? '',
  command: model?.command?.join('\n') ?? '',
  ...memoryFields(model?.memory_mb),
  vgpus: String(model?.vgpus ?? 0),
  auto: model?.auto ?? true,
});

// Mirrors validateModel in internal/deploy/models.go; the backend stays the authority and its
// message is shown when it disagrees.
const NAME_PATTERN = /^[A-Za-z0-9._-]{1,64}$/;

function validate(form: FormState): Errors {
  const errors: Errors = {};
  const name = form.name.trim();
  if (!name) errors.name = 'Enter a name.';
  else if (!NAME_PATTERN.test(name))
    errors.name = "Use up to 64 letters, digits, '.', '_' or '-'.";
  else if (name.toLowerCase() === 'auto')
    errors.name = '"auto" is reserved for automatic model selection.';

  if (!form.reference.trim()) errors.reference = 'Enter the ModelPack reference.';

  const memory = Number(form.memory);
  if (!form.memory.trim()) errors.memory = 'Enter how much memory the engine needs.';
  else if (!Number.isFinite(memory) || toMb(form.memory, form.memoryUnit) <= 0)
    errors.memory = 'Must be more than 0.';
  else if (form.memoryUnit === 'MB' && !Number.isInteger(memory))
    errors.memory = 'Use whole megabytes.';

  const vgpus = Number(form.vgpus);
  if (form.vgpus.trim() && (!Number.isInteger(vgpus) || vgpus < 0))
    errors.vgpus = 'Use a whole number, 0 or more.';
  return errors;
}

const FIELD_IDS: Record<FieldName, string> = {
  name: 'model-name',
  reference: 'model-reference',
  memory: 'model-memory',
  vgpus: 'model-gpus',
};

function engineSummary(form: FormState): string {
  const args = splitLines(form.engineArgs).length;
  const parts = [form.engineImage.trim() || 'Default vLLM image'];
  if (args > 0) parts.push(plural(args, 'extra argument'));
  if (splitLines(form.command).length > 0) parts.push('command override');
  return parts.join(', ');
}

const PLACEHOLDERS = ['{model_path}', '{name}', '{port}', '{api_key}'];

const chip =
  'oai:rounded oai:bg-muted oai:px-1 oai:py-0.5 oai:font-mono oai:text-xs oai:text-foreground';

/** Create dialog, or edit dialog when `model` is set. Remount it (key) to reset the form. */
export function ModelDialog({ model, onClose }: { model?: Model; onClose: () => void }) {
  const [form, setForm] = useState(() => fromModel(model));
  // No errors before the first submit; after that they update while typing.
  const [submitted, setSubmitted] = useState(false);
  // The stored engine_image is never empty (the backend fills in its default), so only args and
  // a command count as "this model has custom engine settings worth showing".
  const [engineOpen, setEngineOpen] = useState(
    () => (model?.engine_args.length ?? 0) > 0 || (model?.command?.length ?? 0) > 0,
  );
  const save = useSaveModel();
  const set = <K extends keyof FormState>(field: K, value: FormState[K]) =>
    setForm((current) => ({ ...current, [field]: value }));

  const errors = submitted ? validate(form) : {};
  const memoryMb = toMb(form.memory, form.memoryUnit);

  function changeUnit(unit: MemoryUnit) {
    // Converting keeps the amount the same; a bare unit flip would silently change 16 GB to 16 MB.
    setForm((current) => {
      if (current.memoryUnit === unit) return current;
      const amount = Number(current.memory);
      if (!current.memory.trim() || !Number.isFinite(amount)) return { ...current, memoryUnit: unit };
      const mb = toMb(current.memory, current.memoryUnit);
      const next = unit === 'MB' ? mb : Math.round((mb / MB_PER_GB) * 100) / 100;
      return { ...current, memoryUnit: unit, memory: String(next) };
    });
  }

  function submit(event: React.FormEvent) {
    event.preventDefault();
    setSubmitted(true);
    const problems = validate(form);
    const first = (Object.keys(FIELD_IDS) as FieldName[]).find((field) => problems[field]);
    if (first) {
      document.getElementById(FIELD_IDS[first])?.focus();
      return;
    }
    const command = splitLines(form.command);
    save.mutate(
      {
        id: model?.id,
        input: {
          name: form.name.trim(),
          reference: form.reference.trim(),
          engine_image: form.engineImage.trim(),
          engine_args: splitLines(form.engineArgs),
          command: command.length > 0 ? command : null,
          memory_mb: memoryMb,
          vgpus: Number(form.vgpus) || 0,
          auto: form.auto,
        },
      },
      { onSuccess: onClose },
    );
  }

  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="oai:flex oai:max-h-[90vh] oai:flex-col oai:gap-0 oai:p-0 oai:sm:max-w-2xl">
        <form className="oai:flex oai:min-h-0 oai:flex-1 oai:flex-col" onSubmit={submit} noValidate>
          <DialogHeader className="oai:border-b oai:px-6 oai:pt-5 oai:pb-4">
            <DialogTitle>{model ? `Edit ${model.name}` : 'Add model'}</DialogTitle>
            <DialogDescription>
              A model is a ModelPack artifact plus the engine that serves it. Place it on nodes on
              the Placement page.
            </DialogDescription>
          </DialogHeader>

          <div className="oai:min-h-0 oai:flex-1 oai:overflow-y-auto oai:px-6 oai:py-5">
            <FieldGroup className="oai:gap-6">
              {save.isError ? (
                <Alert variant="destructive">
                  <AlertCircleIcon aria-hidden />
                  <AlertTitle>Could not save the model</AlertTitle>
                  <AlertDescription>{save.error.message}</AlertDescription>
                </Alert>
              ) : null}

              <FieldSet className="oai:gap-4">
                <FieldLegend>Model</FieldLegend>
                <Field data-invalid={!!errors.name}>
                  <FieldLabel htmlFor="model-name">Name</FieldLabel>
                  <Input
                    id="model-name"
                    autoComplete="off"
                    spellCheck={false}
                    value={form.name}
                    onChange={(e) => set('name', e.target.value)}
                    placeholder="qwen-7b"
                    aria-invalid={!!errors.name}
                    aria-describedby="model-name-hint"
                  />
                  <FieldDescription id="model-name-hint">
                    What clients send as <code className={chip}>model</code>. Also the name the
                    engine serves.
                  </FieldDescription>
                  <FieldError>{errors.name}</FieldError>
                </Field>
                <Field data-invalid={!!errors.reference}>
                  <FieldLabel htmlFor="model-reference">ModelPack reference</FieldLabel>
                  <Input
                    id="model-reference"
                    autoComplete="off"
                    spellCheck={false}
                    className="oai:font-mono oai:text-[13px]"
                    value={form.reference}
                    onChange={(e) => set('reference', e.target.value)}
                    placeholder="registry.example.com/models/qwen:7b"
                    aria-invalid={!!errors.reference}
                    aria-describedby="model-reference-hint"
                  />
                  <FieldDescription id="model-reference-hint">
                    OCI reference of the model artifact, mounted into the engine by the ModelPack
                    CSI driver.
                  </FieldDescription>
                  <FieldError>{errors.reference}</FieldError>
                </Field>
              </FieldSet>

              <FieldSeparator />

              <FieldSet className="oai:gap-4">
                <FieldLegend>Resources</FieldLegend>
                <div className="oai:grid oai:gap-4 oai:sm:grid-cols-[minmax(0,3fr)_minmax(0,2fr)]">
                  <Field data-invalid={!!errors.memory}>
                    <FieldLabel htmlFor="model-memory">Memory</FieldLabel>
                    <div className="oai:flex oai:gap-2">
                      <Input
                        id="model-memory"
                        type="number"
                        inputMode="decimal"
                        min={0}
                        step={form.memoryUnit === 'GB' ? 'any' : 1}
                        placeholder={form.memoryUnit === 'GB' ? '16' : '16384'}
                        value={form.memory}
                        onChange={(e) => set('memory', e.target.value)}
                        aria-invalid={!!errors.memory}
                        aria-describedby="model-memory-hint"
                      />
                      <ToggleGroup
                        type="single"
                        variant="outline"
                        aria-label="Memory unit"
                        value={form.memoryUnit}
                        // Radix reports '' when the active item is clicked again; keep the unit.
                        onValueChange={(unit) => unit && changeUnit(unit as MemoryUnit)}
                        className="oai:shrink-0"
                      >
                        <ToggleGroupItem value="GB" className="oai:px-3">
                          GB
                        </ToggleGroupItem>
                        <ToggleGroupItem value="MB" className="oai:px-3">
                          MB
                        </ToggleGroupItem>
                      </ToggleGroup>
                    </div>
                    <FieldDescription id="model-memory-hint">
                      {form.memoryUnit === 'GB' && memoryMb > 0 && !errors.memory
                        ? `Requests ${memoryMb} MB. A node needs this much free memory.`
                        : 'A node needs this much free memory to take the model.'}
                    </FieldDescription>
                    <FieldError>{errors.memory}</FieldError>
                  </Field>
                  <Field data-invalid={!!errors.vgpus}>
                    <FieldLabel htmlFor="model-gpus">GPUs</FieldLabel>
                    <Input
                      id="model-gpus"
                      type="number"
                      inputMode="numeric"
                      min={0}
                      step={1}
                      value={form.vgpus}
                      onChange={(e) => set('vgpus', e.target.value)}
                      aria-invalid={!!errors.vgpus}
                      aria-describedby="model-gpus-hint"
                    />
                    <FieldDescription id="model-gpus-hint">0 runs on CPU only.</FieldDescription>
                    <FieldError>{errors.vgpus}</FieldError>
                  </Field>
                </div>
                <Field
                  orientation="horizontal"
                  className="oai:items-start oai:rounded-lg oai:border oai:p-4"
                >
                  <FieldContent>
                    <FieldLabel htmlFor="model-auto">Eligible for auto</FieldLabel>
                    <FieldDescription>
                      Requests with <code className={chip}>"model": "auto"</code> may be routed to
                      this model by the scheduler.
                    </FieldDescription>
                  </FieldContent>
                  <Switch
                    id="model-auto"
                    checked={form.auto}
                    onCheckedChange={(v) => set('auto', v)}
                  />
                </Field>
              </FieldSet>

              <FieldSeparator />

              <Collapsible
                open={engineOpen}
                onOpenChange={setEngineOpen}
                className="oai:rounded-lg oai:border"
              >
                <CollapsibleTrigger className="oai:group oai:flex oai:w-full oai:cursor-pointer oai:items-center oai:justify-between oai:gap-3 oai:rounded-lg oai:px-4 oai:py-3 oai:text-left oai:outline-none oai:hover:bg-muted/60 oai:focus-visible:bg-muted/60 oai:data-[state=open]:rounded-b-none oai:data-[state=open]:border-b">
                  <span className="oai:min-w-0">
                    <span className="oai:block oai:text-sm oai:font-medium">Engine</span>
                    <span className="oai:block oai:truncate oai:text-xs oai:text-muted-foreground">
                      {engineOpen
                        ? 'Image, extra arguments and command override.'
                        : engineSummary(form)}
                    </span>
                  </span>
                  <ChevronDownIcon
                    aria-hidden
                    className="oai:size-4 oai:shrink-0 oai:text-muted-foreground oai:transition-transform oai:group-data-[state=open]:rotate-180"
                  />
                </CollapsibleTrigger>
                <CollapsibleContent>
                  <FieldGroup className="oai:gap-5 oai:p-4">
                    <Field>
                      <FieldLabel htmlFor="model-image">Engine image</FieldLabel>
                      <Input
                        id="model-image"
                        autoComplete="off"
                        spellCheck={false}
                        className="oai:font-mono oai:text-[13px]"
                        value={form.engineImage}
                        onChange={(e) => set('engineImage', e.target.value)}
                        placeholder="vllm/vllm-openai"
                        aria-describedby="model-image-hint"
                      />
                      <FieldDescription id="model-image-hint">
                        Leave empty for the default vLLM image.
                      </FieldDescription>
                    </Field>
                    <Field>
                      <FieldLabel htmlFor="model-args">Extra engine arguments</FieldLabel>
                      <Textarea
                        id="model-args"
                        rows={3}
                        spellCheck={false}
                        className="oai:font-mono oai:text-[13px]"
                        value={form.engineArgs}
                        onChange={(e) => set('engineArgs', e.target.value)}
                        placeholder={'--max-model-len\n8192'}
                        aria-describedby="model-args-hint"
                      />
                      <FieldDescription id="model-args-hint">
                        One argument per line, appended to the engine command.
                      </FieldDescription>
                    </Field>
                    <Field>
                      <FieldLabel htmlFor="model-command">Command override</FieldLabel>
                      <Textarea
                        id="model-command"
                        rows={4}
                        spellCheck={false}
                        className="oai:font-mono oai:text-[13px]"
                        value={form.command}
                        onChange={(e) => set('command', e.target.value)}
                        placeholder={'python\n-m\nmy.server\n--model\n{model_path}'}
                        aria-describedby="model-command-hint"
                      />
                      <FieldDescription id="model-command-hint">
                        Optional. Replaces the image entrypoint, one argument per line, starting
                        with the executable. Placeholders:{' '}
                        {PLACEHOLDERS.map((placeholder, index) => (
                          <span key={placeholder}>
                            {index > 0 ? ' ' : null}
                            <code className={chip}>{placeholder}</code>
                          </span>
                        ))}
                      </FieldDescription>
                    </Field>
                  </FieldGroup>
                </CollapsibleContent>
              </Collapsible>
            </FieldGroup>
          </div>

          <DialogFooter className="oai:border-t oai:px-6 oai:py-4">
            <Button type="button" variant="outline" onClick={onClose}>
              Cancel
            </Button>
            <Button type="submit" disabled={save.isPending}>
              {save.isPending ? (
                <>
                  <LoaderCircleIcon aria-hidden className="oai:animate-spin" />
                  Saving...
                </>
              ) : model ? (
                'Save changes'
              ) : (
                'Create model'
              )}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
