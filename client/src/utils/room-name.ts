// Shared with the Add room form; an empty name there still requests a random name.
export const roomNameRules = [
  (value: string) => !value || value.length >= 2 || 'This field must have at least 2 characters',
  (value: string) => !value || !/^[_.-]/.test(value) || 'Cannot start with _ . -',
  (value: string) => !value || /^[a-zA-Z0-9_.-]+$/.test(value) || 'Must only contain a-z A-Z 0-9 _ . -',
]
