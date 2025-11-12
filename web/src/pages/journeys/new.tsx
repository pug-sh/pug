import { useForm } from '@tanstack/react-form'
import { useState } from 'react'
import { z } from 'zod'
import { Button } from '@/components/ui/button'
import { Field, FieldError, FieldGroup, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { Spinner } from '@/components/ui/spinner'
import MobilePreview from '@/components/mobile-preview'
import { journeysService } from '@/lib/rpc'

const formSchema = z.object({
  name: z
    .string()
    .min(2, 'Journey name must be at least 2 characters.')
    .max(100, 'Journey name must not exceed 100 characters.'),
  description: z
    .string()
    .max(500, 'Description must not exceed 500 characters.'), // TODO: make this optional
})

interface JourneyFormProps {
  projectId: string;
  onClose: () => void;
  onSubmitSuccess: () => void;
}

function JourneyForm({ projectId, onClose, onSubmitSuccess }: JourneyFormProps) {
  const [isSubmitting, setIsSubmitting] = useState(false)
  const [formError, setFormError] = useState<string | null>(null)

  const form = useForm({
    defaultValues: {
      name: '',
      description: '',
    },
    validators: {
      onSubmit: formSchema,
    },
    onSubmit: async ({ value }) => {
      setIsSubmitting(true)
      setFormError(null)
      try {        
        await journeysService.create({
          projectId: projectId,
          name: value.name,
          description: value.description,
          state: 2, // STATE_DRAFT (2)
          entryType: 1, // ENTRY_TYPE_SEGMENT (1) - default
          config: new Uint8Array(), // Empty JSON config for now
        })
        onSubmitSuccess()
      } catch (error) {
        const errorMessage = error instanceof Error ? error.message : 'An error occurred creating journey'
        setFormError(errorMessage)
      } finally {
        setIsSubmitting(false)
      }
    },
  })

  return (
    <div className="flex flex-col lg:flex-row lg:space-x-8">
      <div className="lg:w-1/2 w-full">
        <form
          onSubmit={(e) => {
            e.preventDefault()
            form.handleSubmit()
          }}
        >
          {formError && (
            <div className="mb-4 text-sm text-destructive font-normal">
              {formError}
            </div>
          )}
          <FieldGroup>
            <form.Field
              name="name"
              children={(field) => {
                const isInvalid =
                  field.state.meta.isTouched && !field.state.meta.isValid

                return (
                  <Field data-invalid={isInvalid}>
                    <FieldLabel htmlFor={field.name}>Name</FieldLabel>
                    <Input
                      id={field.name}
                      name={field.name}
                      value={field.state.value}
                      onBlur={field.handleBlur}
                      onChange={(e) => field.handleChange(e.target.value)}
                      aria-invalid={isInvalid}
                      placeholder="Name"
                      autoComplete="off"
                    />
                    {isInvalid && (
                      <FieldError errors={field.state.meta.errors} />
                    )}
                  </Field>
                )
              }}
            />
            <form.Field
              name="description"
              children={(field) => {
                const isInvalid =
                  field.state.meta.isTouched && !field.state.meta.isValid

                return (
                  <Field data-invalid={isInvalid}>
                    <FieldLabel htmlFor={field.name}>Description</FieldLabel>
                    <Input
                      id={field.name}
                      name={field.name}
                      value={field.state.value}
                      onBlur={field.handleBlur}
                      onChange={(e) => field.handleChange(e.target.value)}
                      aria-invalid={isInvalid}
                      placeholder="Description"
                      type="text"
                      autoComplete="off"
                    />
                    {isInvalid && (
                      <FieldError errors={field.state.meta.errors} />
                    )}
                  </Field>
                )
              }}
            />
          </FieldGroup>

          <div className="flex justify-end space-x-3 pt-4">
            <Button
              type="button"
              variant="outline"
              onClick={onClose}
            >
              Cancel
            </Button>
            <Button
              type="submit"
              disabled={isSubmitting}
            >
              {isSubmitting ? (
                <>
                  <Spinner className="mr-2 h-4 w-4" />
                  Creating...
                </>
              ) : (
                'Create'
              )}
            </Button>
          </div>
        </form>
      </div>

      {/* Mobile Preview side by side with the form */}
      <div className="mt-8 lg:mt-0 lg:ml-8 flex justify-center">
        <MobilePreview />
      </div>
    </div>
  )
}

export default JourneyForm
