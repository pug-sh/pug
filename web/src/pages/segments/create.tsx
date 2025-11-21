import { useState } from 'react'
import { useLocation } from 'wouter'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Textarea } from '@/components/ui/textarea'
import { segmentsService } from '@/lib/rpc'
import { useAtom } from 'jotai'
import { getSelectedProjectAtom } from '@/atoms/projects'
import { Field, useForm } from '@tanstack/react-form'
import { GroupEditor } from '@/components/segments/group-editor'
import type { FilterGroupUI } from '@/pages/segments/segments'
import { convertToPB, generateId } from '@/lib/segments'


export default function CreateSegment() {
  const [, navigate] = useLocation()
  const [activeProject] = useAtom(getSelectedProjectAtom)

  // Initialize the state for complex nested structure
  const [rootGroup, setRootGroup] = useState<FilterGroupUI>({
    id: generateId(),
    parts: [],
    logicalOperator: 'AND',
    isNested: false
  })

  // Use TanStack Form for simple fields
  const form = useForm({
    defaultValues: {
      name: '',
      description: ''
    },
    onSubmit: async ({ value }) => {
      try {
        const pbFilter = convertToPB(rootGroup)

        await segmentsService.createSegment({
          projectId: activeProject?.id || '',
          name: value.name,
          description: value.description,
          filter: pbFilter
        })

        navigate('/segments')
      } catch (error) {
        console.error('Error creating segment:', error)
      }
    },
  })



  return (
    <div className="container mx-auto py-10">
      <div className="max-w-4xl mx-auto">
        <div className="mb-8">
          <h1 className="text-3xl font-bold tracking-tight">Create New Segment</h1>
          <p className="text-muted-foreground">
            Define criteria to group your users based on metadata and behavior
          </p>
        </div>

        <Card>
          <CardHeader>
            <CardTitle>Segment Details</CardTitle>
          </CardHeader>
          <CardContent>
            <form
              onSubmit={(e) => {
                e.preventDefault();
                e.stopPropagation();
                form.handleSubmit();
              }}
              className="space-y-6"
            >
              <Field
                name="name"
                form={form}
                children={(field) => (
                  <div className="space-y-2">
                    <Label htmlFor={field.name}>Segment Name</Label>
                    <Input
                      id={field.name}
                      placeholder="Enter segment name"
                      value={field.state.value}
                      onChange={(e) => field.handleChange(e.target.value)}
                      required
                    />
                    {field.state.meta.errors && field.state.meta.errors.length > 0 && (
                      <div className="text-destructive text-sm">{field.state.meta.errors[0]}</div>
                    )}
                  </div>
                )}
              />

              <Field
                name="description"
                form={form}
                children={(field) => (
                  <div className="space-y-2">
                    <Label htmlFor={field.name}>Description</Label>
                    <Textarea
                      id={field.name}
                      placeholder="Enter segment description (optional)"
                      value={field.state.value}
                      onChange={(e) => field.handleChange(e.target.value)}
                    />
                  </div>
                )}
              />

              <div className="space-y-4">
                <h3 className="text-lg font-medium">Conditions</h3>
                <GroupEditor
                  group={rootGroup}
                  setGroup={setRootGroup}
                />
              </div>

              <div className="flex justify-end space-x-3 pt-4">
                <Button
                  type="button"
                  variant="outline"
                  onClick={() => navigate('/segments')}
                >
                  Cancel
                </Button>
                <Button type="submit">Create Segment</Button>
              </div>
            </form>
          </CardContent>
        </Card>
      </div>
    </div>
  )
}