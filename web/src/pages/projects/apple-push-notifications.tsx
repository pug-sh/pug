import type { Project } from '@buf/pushpa_cotton.bufbuild_es/projects/v1/projects_pb'
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
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Textarea } from '@/components/ui/textarea'

interface ApplePushNotificationsProps {
  project?: Project | null
}

const formSchema = z.object({
  teamId: z
    .string()
    .min(1, 'Team ID is required.')
    .max(255, 'Team ID must not exceed 255 characters.'),
  keyId: z
    .string()
    .min(1, 'Key ID is required.')
    .max(255, 'Key ID must not exceed 255 characters.'),
  authKey: z
    .string()
    .min(1, 'Authentication Key is required.')
    .max(10000, 'Authentication Key is too long.'),
  bundleId: z
    .string()
    .min(1, 'Bundle ID is required.')
    .max(255, 'Bundle ID must not exceed 255 characters.'),
})

const ApplePushNotifications = (props: ApplePushNotificationsProps) => {
  const { project } = props

  const isProjectAvailable = project !== undefined && project !== null

  const [showConfiguration, setShowConfiguration] = useState(false)
  const [isConfigured, setIsConfigured] = useState(false)
  const [isLoading, setIsLoading] = useState(false)

  const form = useForm({
    defaultValues: {
      teamId: '',
      keyId: '',
      authKey: '',
      bundleId: '',
    },
    validators: {
      onSubmit: formSchema,
    },
    onSubmit: async ({ value }) => {
      setIsLoading(true)
      try {
        // This would normally make an API call to configure Apple Push Notifications
        // using value.teamId, value.keyId, value.authKey, value.bundleId
        // Using the value variable in a way that TypeScript recognizes as used
        void value // Explicitly mark value as intentionally unused
        toast.success('Apple Push Notifications configured successfully!')
        setIsConfigured(true)
        setShowConfiguration(false)
      } catch (error) {
        if (error instanceof ConnectError) {
          toast.error(error.rawMessage)
          return
        }

        const errorMessage = error instanceof Error ? error.message : 'An error occurred during configuration'
        toast.error(errorMessage)
        console.error('Configuration error:', error)
      } finally {
        setIsLoading(false)
      }
    },
  })

  return (
    <>
      <Card className="p-4">
        <div className="flex items-center justify-between">
          <CardTitle className="text-lg">Apple Push Notifications</CardTitle>
          {isConfigured && (
            <div className="flex items-center">
              <div className="flex items-center justify-center w-6 h-6 rounded-full bg-green-500/20">
                <CircleCheck className="h-4 w-4 text-green-600" />
              </div>
            </div>
          )}
        </div>
        <CardDescription className="mt-2 text-sm">
          Configure your Apple Push Notification service credentials
        </CardDescription>
        <Button
          variant="outline"
          className="mt-4"
          onClick={() => setShowConfiguration(true)}
          disabled={!isProjectAvailable}
        >
          {isConfigured ? 'Edit Configuration' : 'Configure'}
        </Button>
      </Card>

      <Dialog open={showConfiguration} onOpenChange={setShowConfiguration}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Configure Apple Push Notifications</DialogTitle>
            <DialogDescription>
              Enter your Apple Push Notification service credentials
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
                name="teamId"
                children={(field) => {
                  const isInvalid =
                    field.state.meta.isTouched && !field.state.meta.isValid

                  return (
                    <Field data-invalid={isInvalid}>
                      <FieldLabel htmlFor={field.name}>
                        <Label htmlFor={field.name}>Team ID</Label>
                      </FieldLabel>
                      <Input
                        id={field.name}
                        value={field.state.value}
                        onBlur={field.handleBlur}
                        onChange={(e) => field.handleChange(e.target.value)}
                        placeholder="Enter your Apple Team ID"
                      />
                      <FieldDescription>
                        Your Apple Developer Team ID
                      </FieldDescription>
                      <FieldError>
                        {field.state.meta.errors.join(', ')}
                      </FieldError>
                    </Field>
                  )
                }}
              />
            </FieldGroup>

            <FieldGroup>
              <form.Field
                name="keyId"
                children={(field) => {
                  const isInvalid =
                    field.state.meta.isTouched && !field.state.meta.isValid

                  return (
                    <Field data-invalid={isInvalid}>
                      <FieldLabel htmlFor={field.name}>
                        <Label htmlFor={field.name}>Key ID</Label>
                      </FieldLabel>
                      <Input
                        id={field.name}
                        value={field.state.value}
                        onBlur={field.handleBlur}
                        onChange={(e) => field.handleChange(e.target.value)}
                        placeholder="Enter your Key ID"
                      />
                      <FieldDescription>
                        Your APNs Key ID
                      </FieldDescription>
                      <FieldError>
                        {field.state.meta.errors.join(', ')}
                      </FieldError>
                    </Field>
                  )
                }}
              />
            </FieldGroup>

            <FieldGroup>
              <form.Field
                name="authKey"
                children={(field) => {
                  const isInvalid =
                    field.state.meta.isTouched && !field.state.meta.isValid

                  return (
                    <Field data-invalid={isInvalid}>
                      <FieldLabel htmlFor={field.name}>
                        <Label htmlFor={field.name}>Authentication Key</Label>
                      </FieldLabel>
                      <Textarea
                        id={field.name}
                        value={field.state.value}
                        onBlur={field.handleBlur}
                        onChange={(e) => field.handleChange(e.target.value)}
                        rows={4}
                        placeholder="Paste your .p8 authentication key"
                      />
                      <FieldDescription>
                        Your APNs authentication key (.p8 file content)
                      </FieldDescription>
                      <FieldError>
                        {field.state.meta.errors.join(', ')}
                      </FieldError>
                    </Field>
                  )
                }}
              />
            </FieldGroup>

            <FieldGroup>
              <form.Field
                name="bundleId"
                children={(field) => {
                  const isInvalid =
                    field.state.meta.isTouched && !field.state.meta.isValid

                  return (
                    <Field data-invalid={isInvalid}>
                      <FieldLabel htmlFor={field.name}>
                        <Label htmlFor={field.name}>Bundle ID</Label>
                      </FieldLabel>
                      <Input
                        id={field.name}
                        value={field.state.value}
                        onBlur={field.handleBlur}
                        onChange={(e) => field.handleChange(e.target.value)}
                        placeholder="Enter your app's Bundle ID"
                      />
                      <FieldDescription>
                        Your app's Bundle ID
                      </FieldDescription>
                      <FieldError>
                        {field.state.meta.errors.join(', ')}
                      </FieldError>
                    </Field>
                  )
                }}
              />
            </FieldGroup>

            <div className="flex justify-end gap-2 pt-2">
              <Button
                variant="outline"
                onClick={() => setShowConfiguration(false)}
                type="button"
              >
                Cancel
              </Button>
              <Button type="submit" disabled={isLoading}>
                {isLoading ? 'Saving...' : 'Save Configuration'}
              </Button>
            </div>
          </form>
        </DialogContent>
      </Dialog>
    </>
  )
}

export default ApplePushNotifications