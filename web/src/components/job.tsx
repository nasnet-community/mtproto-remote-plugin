import type { Job } from '../lib/api'
import { failedAtLink, formatClock, jobStep, jobTitle, stepLabel, stepPercent } from '../lib/format'
import { Circle, CircleCheck, CircleX } from './icons'
import { Bar, ErrorBox, Notice, Spinner, cx } from './ui'

export const DOMESTIC_HINT =
  'This router reaches the server over your domestic internet, so its address must be on the domestic list.'

/** A job's title with its server: "Updating the agent · Helsinki ARM", "new.example.com added". */
export function jobName(j: Job, target: string): string {
  if (j.kind === 'install')
    return j.state === 'ok' ? `${target} added` : j.state === 'running' ? `Installing ${target}` : `Installation failed · ${target}`
  return target ? `${jobTitle(j)} · ${target}` : jobTitle(j)
}

/** "Step 5 of 11 · 2:14 so far", "Took 3:41". */
export function jobLine(job: Job, now: number): string {
  const { n, m } = jobStep(job)
  const start = Date.parse(job.startedAt)
  if (job.state === 'running') {
    const step = m ? `Step ${n} of ${m}` : 'Starting'
    return start ? `${step} · ${formatClock((now - start) / 1000)} so far` : step
  }
  if (job.state === 'failed') return m ? `Failed at step ${n} of ${m}` : 'Failed'
  const end = job.finishedAt ? Date.parse(job.finishedAt) : 0
  return start && end ? `Took ${formatClock((end - start) / 1000)}` : 'Done'
}

const WORD = { ok: 'done', running: 'running', failed: 'failed', pending: 'not started' }

/** A job's steps as they run, pending ones greyed, its error, and the domestic-list hint after a link failure. */
export function JobSteps({ job }: { job: Job | null }) {
  if (!job) return <Spinner />
  const failed = job.steps.find((s) => s.state === 'failed')
  return (
    <div className="flex flex-col gap-4">
      <ol className="flex flex-col gap-3">
        {job.steps.map((step, i) => {
          const pct = step.state === 'running' ? stepPercent(step.detail) : null
          return (
            <li key={i} className="flex gap-3">
              <span className="mt-0.5">
                {step.state === 'running' && <Spinner className="text-info" />}
                {step.state === 'ok' && <CircleCheck className="h-4 w-4 text-ok-text" />}
                {step.state === 'failed' && <CircleX className="h-4 w-4 text-bad-text" />}
                {step.state === 'pending' && <Circle className="h-4 w-4 text-border-strong" />}
              </span>
              <div className="min-w-0 flex-1">
                <div className={cx(step.state === 'running' && 'font-semibold', step.state === 'pending' && 'text-faint')}>
                  {stepLabel(step.name, job.kind)}
                  <span className="sr-only">, {WORD[step.state]}</span>
                </div>
                {step.detail && step.state !== 'pending' && (
                  <div className={cx('whitespace-pre-wrap break-words text-xs', step.state === 'failed' ? 'text-bad-text' : 'text-muted')}>
                    {step.detail}
                  </div>
                )}
                {pct !== null && <Bar pct={pct} className="mt-1.5" />}
              </div>
            </li>
          )
        })}
        {job.state === 'running' && job.steps.length === 0 && (
          <li className="flex gap-3 text-muted">
            <Circle className="h-4 w-4" /> Starting…
          </li>
        )}
      </ol>
      {/* the failed step usually carries the same text already */}
      {job.state === 'failed' && job.error && job.error !== failed?.detail && <ErrorBox>{job.error}</ErrorBox>}
      {failedAtLink(job) && <Notice tone="warn">{DOMESTIC_HINT}</Notice>}
    </div>
  )
}
