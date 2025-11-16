import { UpdateFCMServiceJSONRequestSchema, type Project } from '@buf/pushpa_cotton.bufbuild_es/projects/v1/projects_pb'
import { create } from '@bufbuild/protobuf'
import { ConnectError } from '@connectrpc/connect'
import { useForm } from '@tanstack/react-form'
import { CircleCheck } from 'lucide-react'
import { useState } from 'react'
import { toast } from 'sonner'
import * as z from 'zod'
import { Button } from '@/components/ui/button'
import { Card, CardDescription, CardTitle } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import {
  Field,
  FieldDescription,
  FieldError,
  FieldGroup,
  FieldLabel,
} from '@/components/ui/field'
import { Label } from '@/components/ui/label'
import { Textarea } from '@/components/ui/textarea'
import { projectsService } from '@/lib/rpc'

interface FirebaseIntegrationProps {
  project: Project | null,
  onProjectUpdate?: (updatedProject: Project) => void
}

// Custom validator for JSON
const jsonString = z.string().refine((value) => {
  if (!value.trim()) return true // Allow empty string
  try {
    JSON.parse(value)
    return true
  } catch {
    return false
  }
}, 'Invalid JSON format')

const formSchema = z.object({
  fcmJson: jsonString
    .max(10000, 'Firebase service account JSON is too long.')
})

const FirebaseIntegration = ({ project, onProjectUpdate }: FirebaseIntegrationProps) => {
  const [updating, setUpdating] = useState(false)
  const [showFirebaseConfig, setShowFirebaseConfig] = useState(false)

  const form = useForm({
    defaultValues: {
      fcmJson: project?.fcmServiceJson || '',
    },
    validators: {
      onSubmit: formSchema,
    },
    onSubmit: async ({ value }) => {
      if (!project) return

      setUpdating(true)
      try {
        const request = create(UpdateFCMServiceJSONRequestSchema, {
          fcmServiceJson: value.fcmJson,
          id: project.id
        })

        await projectsService.updateFCMServiceJSON(request)
        // Refresh the project data to reflect the updated FCM JSON value
        const updatedProjectResponse = await projectsService.get({ id: project.id })
        if (updatedProjectResponse.project) {
          // Update the parent component with the new project data to show updated checkmark
          onProjectUpdate?.(updatedProjectResponse.project)
        }
        toast.success('FCM service JSON updated successfully!')
        setShowFirebaseConfig(false)
      } catch (err) {
        if (err instanceof ConnectError) {
          toast.error(err.rawMessage)
          return
        }

        const errorMessage = err instanceof Error ? err.message : 'An error occurred while updating FCM service JSON'
        toast.error(errorMessage)
        console.error('Update FCM error:', err)
      } finally {
        setUpdating(false)
      }
    },
  })

  return (
    <>
      <Card className="p-4">
        <div className="flex items-center justify-between">
          <CardTitle className="text-lg">Firebase</CardTitle>
          {project?.fcmServiceJson && project.fcmServiceJson.trim() !== '' && (
            <div className="flex items-center">
              <div className="flex items-center justify-center w-6 h-6 rounded-full bg-green-500/20">
                <CircleCheck className="h-4 w-4 text-green-600" />
              </div>
            </div>
          )}
        </div>
        <CardDescription className="mt-2 text-sm">
          Configure your Firebase service account for push notifications
        </CardDescription>
        <Button
          variant="outline"
          className="mt-4"
          onClick={() => setShowFirebaseConfig(true)}
        >
          {project?.fcmServiceJson && project.fcmServiceJson.trim() !== ''
            ? 'Edit Configuration'
            : 'Configure'}
        </Button>
      </Card>

      <Dialog open={showFirebaseConfig} onOpenChange={setShowFirebaseConfig}>
        <DialogContent className="max-w-2xl max-h-[90vh] overflow-y-auto">
          <DialogHeader>
            <DialogTitle>Configure Firebase</DialogTitle>
            <DialogDescription>
              Enter your Firebase service account JSON to enable push notifications
            </DialogDescription>
          </DialogHeader>
          <form
            onSubmit={(e) => {
              e.preventDefault()
              form.handleSubmit()
            }}
            className="space-y-4"
          >
            <FieldGroup>
              <form.Field
                name="fcmJson"
                children={(field) => {
                  const isInvalid =
                    field.state.meta.isTouched && !field.state.meta.isValid

                  return (
                    <Field data-invalid={isInvalid}>
                      <FieldLabel htmlFor={field.name}>
                        <Label htmlFor={field.name}>Firebase Service Account JSON</Label>
                      </FieldLabel>
                      <Textarea
                        id={field.name}
                        value={field.state.value}
                        onBlur={field.handleBlur}
                        onChange={(e) => field.handleChange(e.target.value)}
                        rows={8}
                        className="font-mono text-sm"
                        placeholder="Paste your Firebase service account JSON here..."
                      />
                      <FieldDescription>
                        Your Firebase service account JSON configuration
                      </FieldDescription>
                      <FieldError>
                        {field.state.meta.errors.join(', ')}
                      </FieldError>
                    </Field>
                  )
                }}
              />
            </FieldGroup>

            <div className="flex justify-end gap-2">
              <Button
                variant="outline"
                onClick={() => setShowFirebaseConfig(false)}
                type="button"
                disabled={updating}
              >
                Cancel
              </Button>
              <Button type="submit" disabled={updating}>
                {updating ? 'Saving...' : 'Update Firebase Config'}
              </Button>
            </div>
          </form>
        </DialogContent>
      </Dialog>
    </>
  )
}

export default FirebaseIntegration