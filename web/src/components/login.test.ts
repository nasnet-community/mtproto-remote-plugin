import { describe, expect, it } from 'vitest'
import { emptyLogin, keyNeedsTrust, keyTrusted, toLogin, withoutSecrets } from './login'

const probe = (status: 'new' | 'known' | 'changed') => ({ fingerprint: 'SHA256:x', status })

describe('host keys', () => {
  it('trusts a first key and asks only about one that changed', () => {
    expect(keyNeedsTrust(probe('known'))).toBe(false)
    expect(keyNeedsTrust(probe('new'))).toBe(false)
    expect(keyNeedsTrust(probe('changed'))).toBe(true)
  })

  it('needs the box ticked only for a changed key', () => {
    expect(keyTrusted(probe('new'), false)).toBe(true)
    expect(keyTrusted(probe('changed'), false)).toBe(false)
    expect(keyTrusted(probe('changed'), true)).toBe(true)
  })
})

describe('login forms', () => {
  it('fills in the defaults a half-saved server lacks', () => {
    expect(emptyLogin(0, '')).toMatchObject({ sshPort: '22', sshUser: 'root' })
    expect(emptyLogin(2222, 'deploy')).toMatchObject({ sshPort: '2222', sshUser: 'deploy' })
  })

  it('sends the key it was given and only the chosen secret', () => {
    const f = { ...emptyLogin(), password: 'pw', privateKey: 'k' }
    expect(toLogin(f, 'SHA256:new')).toEqual({ sshPort: 22, sshUser: 'root', hostKey: 'SHA256:new', password: 'pw' })
    expect(withoutSecrets(f)).toMatchObject({ password: '', privateKey: '', passphrase: '' })
  })
})
