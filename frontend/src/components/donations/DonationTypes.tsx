import { useEffect, useState } from 'react'
import { Button, Card, Checkbox, Chip } from '@heroui/react'
import { fetchProviders, type ProviderDescriptor } from '@/api/overview'
import { updateSystemSettings, type SystemSettings } from '@/api/system'
import { PageAlert } from '@/components/ui/PageAlert'
import { SkeletonBlock } from '@/components/ui/PageSkeletons'
import { useI18n } from '@/hooks/useI18n'
import { accountProviderLabel } from '@/lib/provider'
import { ProviderMark } from '@/components/ProviderMark'

// One selectable entry: a provider AND a region. Qoder and WorkBuddy each ship a
// CN and a global deployment, and they are separate accounts, so a provider-only
// list would take the global variant along when only the CN one was meant.
type FormOption = { value: string; provider: string; region: string }

function formatOptions(descriptors: ProviderDescriptor[]): FormOption[] {
  const options: FormOption[] = []
  for (const descriptor of descriptors) {
    const regions = descriptor.regions?.length ? descriptor.regions : [{ id: descriptor.default_region }]
    for (const region of regions) {
      if (!region?.id) continue
      options.push({ value: `${descriptor.id}:${region.id}`, provider: descriptor.id, region: region.id })
    }
  }
  return options
}

// DonationTypes lets an operator choose which account types the public
// contribution page offers.
//
// The list is a server-side allow-list, not a page filter: an empty list means
// "everything", and the same rule gates the start/submit endpoints, so hiding a
// format here also refuses a hand-crafted POST for it.
export function DonationTypes({ settings, onSaved }: { settings: SystemSettings | null; onSaved: (settings: SystemSettings) => void }) {
  const { t } = useI18n()
  const [options, setOptions] = useState<FormOption[] | null>(null)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    let active = true
    void fetchProviders().then((result) => {
      if (active) setOptions(formatOptions(result.data || []))
    }).catch((err) => { if (active) setError(err instanceof Error ? err.message : String(err)) })
    return () => { active = false }
  }, [])

  const allowed = settings?.donation_allowed_formats || []
  // An empty allow-list means "all allowed", which the UI shows as everything
  // ticked -- otherwise the operator cannot tell "all" from "none".
  const isOn = (value: string) => allowed.length === 0 || allowed.includes(value)

  async function save(next: string[]) {
    setBusy(true)
    setError('')
    try {
      onSaved(await updateSystemSettings({ donation_allowed_formats: next }))
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  // Rebuild the full list from the visible state rather than patching one entry:
  // the server stores an explicit list, so sending a partial one would drop every
  // other selection. An all-on list is sent as empty, which is the same policy
  // and keeps the stored value canonical.
  //
  // The flip is computed from the current state on purpose. HeroUI v3's Checkbox
  // onChange hands over the change event, not a boolean, so trusting a "selected"
  // argument would read as always-true and every click would re-save "all".
  // Deriving the next value from `!isOn(...)` is correct whatever the signature.
  function toggle(value: string, on: boolean) {
    if (!options) return
    const next = options.map((option) => option.value).filter((v) => (v === value ? on : isOn(v)))
    void save(next.length === options.length ? [] : next)
  }

  return (
    <Card data-gsap-reveal>
      <Card.Header>
        <Card.Title>{t('donationTypesTitle')}</Card.Title>
        <Card.Description>{t('donationTypesHint')}</Card.Description>
      </Card.Header>
      <Card.Content className="space-y-3">
        {error ? <PageAlert title={error} /> : null}
        {!settings || !options ? <SkeletonBlock className="h-24 w-full" /> : (
          <>
            <div className="grid gap-2 sm:grid-cols-2">
              {options.map((option) => (
                <Checkbox
                  key={option.value}
                  isSelected={isOn(option.value)}
                  isDisabled={busy}
                  className="rounded-xl border border-separator px-3 py-2.5 data-selected:border-accent data-selected:bg-accent-soft"
                  onChange={() => toggle(option.value, !isOn(option.value))}
                >
                  <Checkbox.Content className="flex items-center gap-2.5">
                    <Checkbox.Control>
                      <Checkbox.Indicator />
                    </Checkbox.Control>
                    <ProviderMark provider={option.provider} size={16} />
                    <span className="text-sm font-medium">{accountProviderLabel(option.provider, option.region, t)}</span>
                  </Checkbox.Content>
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
