import { useState, useEffect } from 'react'
import { useLocation, useParams } from 'wouter'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Textarea } from '@/components/ui/textarea'
import { segmentsService } from '@/lib/rpc'
import type { Condition, SegmentFilter } from '@buf/pushpa_cotton.bufbuild_es/segments/v1/segments_pb'
import { create } from '@bufbuild/protobuf'
import { ConditionSchema, FilterPartSchema, SegmentFilterSchema } from '@buf/pushpa_cotton.bufbuild_es/segments/v1/segments_pb'
import { Plus, Minus } from 'lucide-react'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select';
import type { ConditionUI, FilterGroupUI } from '@/pages/segments/segments'
import { Field, useForm } from '@tanstack/react-form'
import { generateId } from '@/lib/segments'


// Helper function to convert protobuf SegmentFilter to UI structure
function pbToUI(filter: SegmentFilter): FilterGroupUI {
  const parts = filter.parts?.map(part => {
    // Handle the oneof field properly by checking if condition is set
    if (part.part.case === 'condition' && part.part.value) {
      // It's a condition
      const condition = part.part.value as Condition;
      return {
        id: generateId(),
        field: condition.field,
        operator: condition.operator,
        value: condition.value
      } as ConditionUI
    } else if (part.part.case === 'subFilter' && part.part.value) {
      // It's a sub-filter
      const subFilter = part.part.value as SegmentFilter;
      return pbToUI(subFilter) // Recursive call for nested structure
    }
    return null
  }).filter(Boolean) as (ConditionUI | FilterGroupUI)[]

  return {
    id: generateId(),
    parts,
    logicalOperator: (filter.logicalOperator as 'AND' | 'OR') || 'AND',
    isNested: false
  }
}

// Helper function to convert UI structure to protobuf
function uiToPB(group: FilterGroupUI): SegmentFilter {
  const parts = group.parts.map(part => {
    if ('isNested' in part && part.isNested) {
      // It's a sub-group
      const subFilter = uiToPB(part as FilterGroupUI)
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

export default function EditSegment() {
  const [, navigate] = useLocation()
  const { id } = useParams<{ id: string }>()

  // Initialize the state for complex nested structure
  const [rootGroup, setRootGroup] = useState<FilterGroupUI>({
    id: generateId(),
    parts: [],
    logicalOperator: 'AND',
    isNested: false
  })
  const [isActive, setIsActive] = useState(true);

  // Use TanStack Form for simple fields
  const form = useForm({
    defaultValues: {
      name: '',
      description: ''
    },
    onSubmit: async ({ value }) => {
      try {
        const pbFilter = uiToPB(rootGroup)

        await segmentsService.updateSegment({
          segmentId: id,
          name: value.name,
          description: value.description,
          filter: pbFilter,
          isActive: isActive
        })

        navigate('/segments')
      } catch (error) {
        console.error('Error updating segment:', error)
      }
    },
  })
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    if (id) {
      fetchSegment()
    }
  }, [id])

  const fetchSegment = async () => {
    try {
      setLoading(true)
      const response = await segmentsService.getSegment({ segmentId: id })
      const segment = response.segment

      if (segment) {
        // Update the form values using TanStack Form
        form.setFieldValue('name', segment.name);
        form.setFieldValue('description', segment.description);

        // Update the local state
        setIsActive(segment.isActive);
        setRootGroup(segment.filter ? pbToUI(segment.filter) : {
          id: generateId(),
          parts: [],
          logicalOperator: 'AND',
          isNested: false
        });
      }
    } catch (error) {
      console.error('Error fetching segment:', error)
    } finally {
      setLoading(false)
    }
  }

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
            ('id' in part && part.id !== partId)
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
            <Select
              value={group.logicalOperator}
              onValueChange={(value) => updateGroupOperator(group.id, value as 'AND' | 'OR')}
            >
              <SelectTrigger className="w-[80px]">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="AND">AND</SelectItem>
                <SelectItem value="OR">OR</SelectItem>
              </SelectContent>
            </Select>
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
                  <Select
                    value={condition.operator}
                    onValueChange={(value) => {
                      const updateGroup = (g: FilterGroupUI): FilterGroupUI => {
                        if (g.id === group.id) {
                          const updatedParts = g.parts.map(p => {
                            if ('id' in p && p.id === condition.id) {
                              return { ...p, operator: value }
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
                  >
                    <SelectTrigger className="w-[180px]">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="EQUALS">equals</SelectItem>
                      <SelectItem value="NOT_EQUALS">not equals</SelectItem>
                      <SelectItem value="CONTAINS">contains</SelectItem>
                      <SelectItem value="NOT_CONTAINS">does not contain</SelectItem>
                      <SelectItem value="GREATER_THAN">greater than</SelectItem>
                      <SelectItem value="LESS_THAN">less than</SelectItem>
                    </SelectContent>
                  </Select>
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

  if (loading) {
    return (
      <div className="container mx-auto py-10 flex justify-center items-center">
        <div className="animate-spin rounded-full h-10 w-10 border-b-2 border-gray-900"></div>
      </div>
    )
  }

  return (
    <div className="container mx-auto py-10">
      <div className="max-w-4xl mx-auto">
        <div className="mb-8">
          <h1 className="text-3xl font-bold tracking-tight">Edit Segment</h1>
          <p className="text-muted-foreground">
            Modify the criteria for this user segment
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

              <div className="flex items-center space-x-2 pt-4">
                <input
                  type="checkbox"
                  id="isActive"
                  checked={isActive}
                  onChange={(e) => setIsActive(e.target.checked)}
                  className="h-4 w-4"
                />
                <Label htmlFor="isActive">Active</Label>
              </div>

              <div className="flex justify-end space-x-3 pt-4">
                <Button
                  type="button"
                  variant="outline"
                  onClick={() => navigate('/segments')}
                >
                  Cancel
                </Button>
                <Button type="submit">Update Segment</Button>
              </div>
            </form>
          </CardContent>
        </Card>
      </div>
    </div>
  )
}