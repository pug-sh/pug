import { atom } from 'jotai'
import { atomWithStorage } from 'jotai/utils'
import { stringStorage } from './utils'
import { projectsService } from '@/lib/rpc'

export const selectedProjectAtom = atomWithStorage('selectedProject', '', stringStorage, { getOnInit: true })

export const dialogOpenAtom = atom(false)

export const batchGetAtom = atom(async () => {
  const { projects } = await projectsService.batchGet({})
  return projects
})

export const getProjectByIdAtom = atom(async (id: string) => {
  const { project } = await projectsService.get({ id })
  return project
})

export const getSelectedProjectAtom = atom(async get => {
  const selectedProject = get(selectedProjectAtom)
  if (!selectedProject) {
    throw new Error('No project selected')
  }
  const { project } = await projectsService.get({ id: selectedProject })
  return project
})
