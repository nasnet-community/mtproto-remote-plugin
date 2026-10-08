import { useCallback, useEffect, useRef, useState } from 'react'
import { followJob, type Job } from '../lib/api'

/** Follows a job by id; `onDone` runs once when it ends. */
export function useJob(jobId: string | null, onDone?: (job: Job) => void) {
  const [job, setJob] = useState<Job | null>(null)
  const done = useRef(onDone)
  done.current = onDone

  useEffect(() => {
    setJob(null)
    if (!jobId) return
    return followJob(jobId, (j) => {
      setJob(j)
      if (j.state !== 'running') done.current?.(j)
    })
  }, [jobId])

  return job
}

/**
 * Follows one job at a time past its dialog and its page: `onEnd` still runs
 * (toast, reload) when nobody is watching any more.
 */
export function useBackgroundJob<K extends Job['kind']>(onEnd: (job: Job) => void) {
  const [followed, setFollowed] = useState<{ jobId: string; kind: K; job: Job | null } | null>(null)
  const end = useRef(onEnd)
  end.current = onEnd

  const follow = useCallback((jobId: string, kind: K) => {
    setFollowed({ jobId, kind, job: null })
    // never stopped on unmount, on purpose; it stops itself when the job ends
    followJob(jobId, (job) => {
      setFollowed((f) => (f?.jobId === jobId ? { ...f, job } : f))
      if (job.state !== 'running') end.current(job)
    })
  }, [])

  const running = followed !== null && (followed.job === null || followed.job.state === 'running')
  return { followed, follow, running }
}
