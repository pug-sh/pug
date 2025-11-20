import { useState } from 'react'
import { useLocation } from 'wouter'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Textarea } from '@/components/ui/textarea'
import { segmentsService } from '@/lib/rpc'
import type { SegmentFilter } from '@buf/pushpa_cotton.bufbuild_es/segments/v1/segments_pb'
import { create } from '@bufbuild/protobuf'
import { ConditionSchema, FilterPartSchema, SegmentFilterSchema } from '@buf/pushpa_cotton.bufbuild_es/segments/v1/segments_pb'
import { useAtom } from 'jotai'
import { getSelectedProjectAtom } from '@/atoms/projects'
import { Plus, Minus } from 'lucide-react'
import { Field, useForm } from '@tanstack/react-form'

interface ConditionUI {
  id: string
  field: string
  operator: string
  value: string
}

interface FilterGroupUI {
  id: string
  parts: (ConditionUI | FilterGroupUI)[]
  logicalOperator: 'AND' | 'OR'
  isNested: boolean
}


function generateId() {
  return Math.random().toString(36).substring(2, 9)
}

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

  const addConditionToGroup = (groupId: string, condition: ConditionUI) => {
    const updateGroup = (group: FilterGroupUI): FilterGroupUI => {
      if (group.id === groupId) {
        return {
          ...group,
          parts: [...group.parts, condition]
        }
      }

      return {
        ...group,
        parts: group.parts.map(part =>
          'isNested' in part ? updateGroup(part as FilterGroupUI) : part
        )
      }
    }

    setRootGroup(prev => updateGroup(prev))
  }

  const addSubGroupToGroup = (groupId: string, subGroup: FilterGroupUI) => {
    const updateGroup = (group: FilterGroupUI): FilterGroupUI => {
      if (group.id === groupId) {
        return {
          ...group,
          parts: [...group.parts, subGroup]
        }
      }

      return {
        ...group,
        parts: group.parts.map(part =>
          'isNested' in part ? updateGroup(part as FilterGroupUI) : part
        )
      }
    }

    setRootGroup(prev => updateGroup(prev))
  }

  const removePartFromGroup = (groupId: string, partId: string) => {
    const updateGroup = (group: FilterGroupUI): FilterGroupUI => {
      if (group.id === groupId) {
        return {
          ...group,
          parts: group.parts.filter(part =>
            ('id' in part && part.id !== partId) ||
            ('field' in part && part.field !== partId)
          )
        }
      }

      return {
        ...group,
        parts: group.parts.map(part =>
          'isNested' in part ? updateGroup(part as FilterGroupUI) : part
        )
      }
    }

    setRootGroup(prev => updateGroup(prev))
  }

  const updateGroupOperator = (groupId: string, operator: 'AND' | 'OR') => {
    const updateGroup = (group: FilterGroupUI): FilterGroupUI => {
      if (group.id === groupId) {
        return {
          ...group,
          logicalOperator: operator
        }
      }

      return {
        ...group,
        parts: group.parts.map(part =>
          'isNested' in part ? updateGroup(part as FilterGroupUI) : part
        )
      }
    }

    setRootGroup(prev => updateGroup(prev))
  }

  const addNewCondition = (groupId: string) => {
    const newCondition: ConditionUI = {
      id: generateId(),
      field: '',
      operator: 'EQUALS',
      value: ''
    }
    addConditionToGroup(groupId, newCondition)
  }

  const addNewSubGroup = (groupId: string) => {
    const newSubGroup: FilterGroupUI = {
      id: generateId(),
      parts: [],
      logicalOperator: 'AND',
      isNested: true
    }
    addSubGroupToGroup(groupId, newSubGroup)
  }

  const renderGroup = (group: FilterGroupUI, level: number = 0) => {
    const isRootGroup = level === 0

    return (
      <div
        key={group.id}
        className={`p-4 rounded-lg ${isRootGroup ? 'bg-white' : 'bg-gray-50'} border`}
      >
        <div className="flex items-center justify-between mb-3">
          <h3 className="font-medium">
            {isRootGroup ? 'Main Group' : `Nested Group ${level}`}
          </h3>
          <div className="flex items-center space-x-2">
            <span className="text-sm text-muted-foreground">Operator:</span>
            <select
              value={group.logicalOperator}
              onChange={(e) => updateGroupOperator(group.id, e.target.value as 'AND' | 'OR')}
              className="border rounded px-2 py-1 text-sm"
            >
              <option value="AND">AND</option>
              <option value="OR">OR</option>
            </select>
          </div>
        </div>

        <div className="space-y-3">
          {group.parts.map((part, _index) => {
            if ('isNested' in part) {
              // It's a sub-group
              return (
                <div key={part.id} className="ml-4 pl-4 border-l-2 border-gray-300">
                  {renderGroup(part as FilterGroupUI, level + 1)}
                  <div className="mt-2 flex justify-end">
                    <Button
                      type="button"
                      variant="outline"
                      size="sm"
                      onClick={() => removePartFromGroup(group.id, part.id)}
                    >
                      <Minus className="h-4 w-4" />
                    </Button>
                  </div>
                </div>
              )
            } else {
              // It's a condition
              const condition = part as ConditionUI
              return (
                <div
                  key={condition.id}
                  className="flex items-center space-x-2 p-2 bg-white rounded border"
                >
                  <Input
                    placeholder="Field (e.g. gender)"
                    value={condition.field}
                    onChange={(e) => {
                      const updateGroup = (g: FilterGroupUI): FilterGroupUI => {
                        if (g.id === group.id) {
                          const updatedParts = g.parts.map(p => {
                            if ('id' in p && p.id === condition.id) {
                              return { ...p, field: e.target.value }
                            }
                            return p
                          })
                          return { ...g, parts: updatedParts }
                        }

                        return {
                          ...g,
                          parts: g.parts.map(p =>
                            'isNested' in p ? updateGroup(p as FilterGroupUI) : p
                          )
                        }
                      }

                      setRootGroup(prev => updateGroup(prev))
                    }}
                  />
                  <select
                    value={condition.operator}
                    onChange={(e) => {
                      const updateGroup = (g: FilterGroupUI): FilterGroupUI => {
                        if (g.id === group.id) {
                          const updatedParts = g.parts.map(p => {
                            if ('id' in p && p.id === condition.id) {
                              return { ...p, operator: e.target.value }
                            }
                            return p
                          })
                          return { ...g, parts: updatedParts }
                        }

                        return {
                          ...g,
                          parts: g.parts.map(p =>
                            'isNested' in p ? updateGroup(p as FilterGroupUI) : p
                          )
                        }
                      }

                      setRootGroup(prev => updateGroup(prev))
                    }}
                    className="border rounded px-2 py-2"
                  >
                    <option value="EQUALS">equals</option>
                    <option value="NOT_EQUALS">not equals</option>
                    <option value="CONTAINS">contains</option>
                    <option value="NOT_CONTAINS">does not contain</option>
                    <option value="GREATER_THAN">greater than</option>
                    <option value="LESS_THAN">less than</option>
                  </select>
                  <Input
                    placeholder="Value"
                    value={condition.value}
                    onChange={(e) => {
                      const updateGroup = (g: FilterGroupUI): FilterGroupUI => {
                        if (g.id === group.id) {
                          const updatedParts = g.parts.map(p => {
                            if ('id' in p && p.id === condition.id) {
                              return { ...p, value: e.target.value }
                            }
                            return p
                          })
                          return { ...g, parts: updatedParts }
                        }

                        return {
                          ...g,
                          parts: g.parts.map(p =>
                            'isNested' in p ? updateGroup(p as FilterGroupUI) : p
                          )
                        }
                      }

                      setRootGroup(prev => updateGroup(prev))
                    }}
                  />
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    onClick={() => removePartFromGroup(group.id, condition.id)}
                  >
                    <Minus className="h-4 w-4" />
                  </Button>
                </div>
              )
            }
          })}

          <div className="flex space-x-2 mt-3">
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={() => addNewCondition(group.id)}
            >
              <Plus className="h-4 w-4 mr-1" />
              Add Condition
            </Button>
            {level < 3 && ( // Limit nesting to 3 levels to prevent infinite nesting
              <Button
                type="button"
                variant="outline"
                size="sm"
                onClick={() => addNewSubGroup(group.id)}
              >
                <Plus className="h-4 w-4 mr-1" />
                Add Group
              </Button>
            )}
          </div>
        </div>
      </div>
    )
  }

  const convertToPB = (group: FilterGroupUI): SegmentFilter => {
    const parts = group.parts.map(part => {
      if ('isNested' in part && part.isNested) {
        // It's a sub-group
        const subFilter = convertToPB(part as FilterGroupUI)
        const filterPart = create(FilterPartSchema)
        filterPart.part = { case: 'subFilter', value: subFilter }
        return filterPart
      } else {
        // It's a condition
        const condition = create(ConditionSchema, {
          field: (part as ConditionUI).field,
          operator: (part as ConditionUI).operator,
          value: (part as ConditionUI).value
        })
        const filterPart = create(FilterPartSchema)
        filterPart.part = { case: 'condition', value: condition }
        return filterPart
      }
    })

    return create(SegmentFilterSchema, {
      parts: parts,
      logicalOperator: group.logicalOperator
    })
  }


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
                {renderGroup(rootGroup)}
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