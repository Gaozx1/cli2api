import { useEffect, useState } from 'react'
import { Button, Card, Checkbox, Chip } from '@heroui/react'
import { fetchProviders, type ProviderDescriptor } from '@/api/overview'
import { updateSystemSettings, type SystemSettings } from '@/api/system'
import { PageAlert } from '@/components/ui/PageAlert'
import { SkeletonBlock } from '@/components/ui/PageSkeletons'
import { useI18n } from '@/hooks/useI18n'

// DonationTypes lets an operator choose which account types the public
// contribution page offers.
//
// The list is a server-side allow-list, not a page filter: an empty list means
// "everything", and the same rule gates the start/submit endpoints, so hiding a
// provider here also refuses a hand-crafted POST for it.
export function DonationTypes({ settings, onSaved }: { settings: SystemSettings | null; onSaved: (settings: SystemSettings) => void }) {
  const { t } = useI18n()
  const [providers, setProviders] = useState<ProviderDescriptor[] | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  useEffect(() => {
    let active = true
    void fetchProviders().then((result) => {
      if (active) setProviders(result.data || [])
    }).catch((err) => { if (active) setError(String(err)) })
    return () => { active = false }
  }, [])

  const allowed = settings?.donation_allowed_providers || []
  // An empty allow-list means "all allowed", which the UI shows as everything
  // ticked -- otherwise the operator cannot tell "all" from "none".
  const isOn = (id: string) => allowed.length === 0 || allowed.includes(id)

  async function save(next: string[]) {
    setBusy(true)
    setError('')
    try {
      onSaved(await updateSystemSettings({ donation_allowed_providers: next }))
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  // Rebuild the full list from the visible state rather than patching one id:
  // the server stores an explicit list, so sending a partial one would drop
  // every other selection. An all-on list is sent as empty, which is the same
  // policy and keeps the stored value canonical.
  function toggle(id: string, on: boolean) {
    if (!providers) return
    const next = providers.map((provider) => provider.id).filter((pid) => (pid === id ? on : isOn(pid)))
    void save(next.length === providers.length ? [] : next)
  }

  return (
    <Card data-gsap-reveal>
      <Card.Header>
        <Card.Title>{t('donationTypesTitle')}</Card.Title>
        <Card.Description>{t('donationTypesHint')}</Card.Description>
      </Card.Header>
      <Card.Content className="space-y-3">
        {error ? <PageAlert title={error} /> : null}
        {!settings || !providers ? <SkeletonBlock className="h-24 w-full" /> : (
          <>
            <div className="flex flex-wrap gap-x-5 gap-y-2">
              {providers.map((provider) => (
                <Checkbox
                  key={provider.id}
                  isSelected={isOn(provider.id)}
                  isDisabled={busy}
                  onChange={(on: boolean) => toggle(provider.id, on)}
                >
                  {provider.label}
                </Checkbox>
              ))}
            </div>
            {allowed.length === 0 ? <Chip size="sm" variant="soft">{t('donationTypesAll')}</Chip> : null}
          </>
        )}
      </Card.Content>
      <Card.Footer>
        <Button variant="secondary" isDisabled={busy || !settings} onPress={() => void save([])}>
          {t('donationTypesReset')}
        </Button>
      </Card.Footer>
    </Card>
  )
}
