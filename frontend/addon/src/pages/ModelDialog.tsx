import {
  Button,
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  Input,
  Label,
  Switch,
  Textarea,
} from '@oakestra/ui';
import { useState } from 'react';

import { useSaveModel } from '../api';
import { splitLines } from '../lib';
import type { Model } from '../types';

interface FormState {
  name: string;
  reference: string;
  engineImage: string;
  engineArgs: string;
  command: string;
  memoryMb: string;
  vgpus: string;
  auto: boolean;
}

const fromModel = (model?: Model): FormState => ({
  name: model?.name ?? '',
  reference: model?.reference ?? '',
  engineImage: model?.engine_image ?? '',
  engineArgs: model?.engine_args.join('\n') ?? '',
  command: model?.command?.join('\n') ?? '',
  memoryMb: String(model?.memory_mb ?? 0),
  vgpus: String(model?.vgpus ?? 0),
  auto: model?.auto ?? true,
});

function Field({ id, label, hint, children }: { id: string; label: string; hint?: string; children: React.ReactNode }) {
  return (
    <div className="oai:space-y-1.5">
      <Label htmlFor={id}>{label}</Label>
      {children}
      {hint ? <p className="oai:text-xs oai:text-muted-foreground">{hint}</p> : null}
    </div>
  );
}

/** Create dialog, or edit dialog when `model` is set. Remount it (key) to reset the form. */
export function ModelDialog({ model, onClose }: { model?: Model; onClose: () => void }) {
  const [form, setForm] = useState(() => fromModel(model));
  const save = useSaveModel();
  const set = <K extends keyof FormState>(field: K, value: FormState[K]) =>
    setForm((current) => ({ ...current, [field]: value }));

  function submit(event: React.FormEvent) {
    event.preventDefault();
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
          memory_mb: Number(form.memoryMb) || 0,
          vgpus: Number(form.vgpus) || 0,
          auto: form.auto,
        },
      },
      { onSuccess: onClose },
    );
  }

  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="oai:max-h-[90vh] oai:overflow-y-auto">
        <form className="oai:space-y-4" onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>{model ? `Edit ${model.name}` : 'Add model'}</DialogTitle>
            <DialogDescription>
              A model is a ModelPack artifact plus the engine that serves it.
            </DialogDescription>
          </DialogHeader>
          <Field id="model-name" label="Name" hint="The value clients pass as `model`.">
            <Input id="model-name" value={form.name} onChange={(e) => set('name', e.target.value)} required />
          </Field>
          <Field id="model-reference" label="ModelPack reference">
            <Input
              id="model-reference"
              value={form.reference}
              onChange={(e) => set('reference', e.target.value)}
              placeholder="registry.example.com/models/qwen:7b"
              required
            />
          </Field>
          <Field id="model-image" label="Engine image" hint="Leave empty to use the default vLLM image.">
            <Input
              id="model-image"
              value={form.engineImage}
              onChange={(e) => set('engineImage', e.target.value)}
              placeholder="vllm/vllm-openai (default)"
            />
          </Field>
          <Field id="model-args" label="Extra engine arguments" hint="One argument per line.">
            <Textarea
              id="model-args"
              rows={3}
              value={form.engineArgs}
              onChange={(e) => set('engineArgs', e.target.value)}
              placeholder={'--max-model-len\n8192'}
            />
          </Field>
          <Field
            id="model-command"
            label="Command override (optional)"
            hint="Replaces the image entrypoint, one argument per line. Placeholders: {model_path} {name} {port} {api_key}."
          >
            <Textarea
              id="model-command"
              rows={3}
              value={form.command}
              onChange={(e) => set('command', e.target.value)}
              placeholder={'python\n-m\nmy.server\n--model\n{model_path}'}
            />
          </Field>
          <div className="oai:grid oai:grid-cols-2 oai:gap-4">
            <Field id="model-memory" label="Memory (MB)">
              <Input
                id="model-memory"
                type="number"
                min={0}
                value={form.memoryMb}
                onChange={(e) => set('memoryMb', e.target.value)}
              />
            </Field>
            <Field id="model-gpus" label="GPUs">
              <Input
                id="model-gpus"
                type="number"
                min={0}
                value={form.vgpus}
                onChange={(e) => set('vgpus', e.target.value)}
              />
            </Field>
          </div>
          <div className="oai:flex oai:items-center oai:gap-2">
            <Switch id="model-auto" checked={form.auto} onCheckedChange={(v) => set('auto', v)} />
            <Label htmlFor="model-auto">Eligible for auto</Label>
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>
              Cancel
            </Button>
            <Button type="submit" disabled={save.isPending}>
              {model ? 'Save' : 'Create model'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
