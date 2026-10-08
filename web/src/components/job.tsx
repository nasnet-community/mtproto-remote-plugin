import type { Job } from '../lib/api'
import { failedAtLink, stepLabel } from '../lib/format'
import { Circle, CircleCheck, CircleX } from './icons'
import { ErrorBox, Notice, Spinner, cx } from './ui'

export const DOMESTIC_HINT =
  "This router reaches the server over its domestic route, so the server's address must be in the domestic address list."

/** A job's steps as they run, its error, and the domestic-list hint after a link failure. */
export function JobSteps({ job }: { job: Job | null }) {
  if (!job) return <Spinner />
  const failed = job.steps.find((s) => s.state === 'failed')
  return (
    <div className="flex flex-col gap-4">
      <ol className="flex flex-col gap-3">
        {job.steps.map((step) => (
          <li key={step.name} className="flex gap-3">
            <span className="mt-0.5">
              {step.state === 'running' && <Spinner className="text-accent" />}
              {step.state === 'ok' && <CircleCheck className="h-4 w-4 text-status-online" />}
              {step.state === 'failed' && <CircleX className="h-4 w-4 text-status-danger" />}
            </span>
            <div className="min-w-0">
              <div className={cx(step.state === 'running' && 'font-medium')}>
                {stepLabel(step.name, job.kind)}
              </div>
              {step.detail && (
                <div
                  className={cx(
                    'whitespace-pre-wrap break-words text-xs',
                    step.state === 'failed' ? 'text-status-danger' : 'text-muted',
                  )}
                >
                  {step.detail}
                </div>
              )}
            </div>
          </li>
        ))}
        {job.state === 'running' && job.steps.length === 0 && (
          <li className="flex gap-3 text-muted">
            <Circle className="h-4 w-4" /> Starting…
          </li>
        )}
      </ol>
      {/* the failed step usually carries the same text already */}
      {job.state === 'failed' && job.error && job.error !== failed?.detail && (
        <ErrorBox>{job.error}</ErrorBox>
      )}
      {failedAtLink(job) && <Notice tone="warn">{DOMESTIC_HINT}</Notice>}
    </div>
  )
}
