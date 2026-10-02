export const defaultPassword = 'CHANGEME'
export const minPasswordLength = 12

export interface PasswordRule {
  label: string
  met: boolean
}

export function passwordRules(next: string, confirm: string): PasswordRule[] {
  return [
    {
      label: `At least ${minPasswordLength} characters`,
      met: next.length >= minPasswordLength,
    },
    {
      label: 'Not the default password',
      met: next !== '' && next !== defaultPassword,
    },
    { label: 'Both entries match', met: next !== '' && next === confirm },
  ]
}

/** Returns why the new password cannot be used, or null. */
export function validateNewPassword(
  next: string,
  confirm: string,
): string | null {
  if (next === defaultPassword)
    return 'Choose a password other than the default.'
  if (next.length < minPasswordLength)
    return `Use at least ${minPasswordLength} characters.`
  if (next !== confirm) return 'The passwords do not match.'
  return null
}
