// Minimal runtime decoders for API responses. A response that does not match
// its decoder is rejected instead of being trusted by the UI.

export class DecodeError extends Error {
  constructor(readonly path: string) {
    super(`unexpected value at ${path || 'response'}`)
    this.name = 'DecodeError'
  }
}

export type Decoder<T> = (value: unknown, path?: string) => T
export type Decoded<D> = D extends Decoder<infer T> ? T : never

export const str: Decoder<string> = (value, path = '') => {
  if (typeof value !== 'string') throw new DecodeError(path)
  return value
}

export const num: Decoder<number> = (value, path = '') => {
  if (typeof value !== 'number' || !Number.isFinite(value)) throw new DecodeError(path)
  return value
}

export const bool: Decoder<boolean> = (value, path = '') => {
  if (typeof value !== 'boolean') throw new DecodeError(path)
  return value
}

export function oneOf<const T extends string>(...allowed: T[]): Decoder<T> {
  return (value, path = '') => {
    if (typeof value !== 'string' || !(allowed as string[]).includes(value)) throw new DecodeError(path)
    return value as T
  }
}

export function nullable<T>(decoder: Decoder<T>): Decoder<T | null> {
  return (value, path) => (value === null || value === undefined ? null : decoder(value, path))
}

export function optional<T>(decoder: Decoder<T>): Decoder<T | undefined> {
  return (value, path) => (value === undefined ? undefined : decoder(value, path))
}

export function arrayOf<T>(decoder: Decoder<T>): Decoder<T[]> {
  return (value, path = '') => {
    if (!Array.isArray(value)) throw new DecodeError(path)
    return value.map((item, index) => decoder(item, `${path}[${index}]`))
  }
}

export const stringMap: Decoder<Record<string, string>> = (value, path = '') => {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) throw new DecodeError(path)
  const out: Record<string, string> = {}
  for (const [key, item] of Object.entries(value)) out[key] = str(item, `${path}.${key}`)
  return out
}

type Shape = Record<string, Decoder<unknown>>

export function object<S extends Shape>(shape: S): Decoder<{ [K in keyof S]: Decoded<S[K]> }> {
  return (value, path = '') => {
    if (typeof value !== 'object' || value === null || Array.isArray(value)) throw new DecodeError(path)
    const record = value as Record<string, unknown>
    const out: Record<string, unknown> = {}
    for (const [key, decoder] of Object.entries(shape)) {
      out[key] = decoder(record[key], path ? `${path}.${key}` : key)
    }
    return out as { [K in keyof S]: Decoded<S[K]> }
  }
}
