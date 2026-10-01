import { tmpdir } from 'node:os'
import { join } from 'node:path'

/** Where the throwaway server listening on port keeps its database. */
export const dataDir = (port: number) => join(tmpdir(), `webpty-e2e-${port}`)
